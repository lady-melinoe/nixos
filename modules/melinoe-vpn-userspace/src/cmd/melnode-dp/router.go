package main

import (
	"net"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/tun"

	"melnode/dpproto"
)

type Router struct {
	dev *Device

	localID uint32

	peerLocks map[uint32]*sync.Mutex

	mu         sync.RWMutex
	tuns       map[uint32]*tunEntry
	routeTable map[uint32]uint32
}

type tunEntry struct {
	dev    tun.Device
	name   string
	writer *tunWriter
}

func newRouter(dev *Device, localID uint32) *Router {
	return &Router{
		dev:        dev,
		localID:    localID,
		peerLocks:  make(map[uint32]*sync.Mutex),
		tuns:       make(map[uint32]*tunEntry),
		routeTable: make(map[uint32]uint32),
	}
}

func (r *Router) peerLock(peerID uint32) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.peerLocks[peerID]
	if !ok {
		l = &sync.Mutex{}
		r.peerLocks[peerID] = l
	}
	return l
}

func (r *Router) LookupRoute(dstPeerID uint32) (nhid uint32, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	nhid, ok = r.routeTable[dstPeerID]
	return
}

func (r *Router) LookupTunWriter(peerID uint32) (*tunWriter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tuns[peerID]
	if !ok || t.writer == nil {
		return nil, false
	}
	return t.writer, true
}

func (r *Router) ReplaceRoutes(routes []dpproto.Route) error {
	table := make(map[uint32]uint32, len(routes))
	for _, rt := range routes {
		if rt.Dst > 255 || rt.NextHop > 255 {
			return dpproto.Errorf(dpproto.CodeInvalid, "route %d via %d: peerids are 0-255", rt.Dst, rt.NextHop)
		}
		if _, dup := table[rt.Dst]; dup {
			return dpproto.Errorf(dpproto.CodeInvalid, "duplicate route for destination %d", rt.Dst)
		}
		table[rt.Dst] = rt.NextHop
	}
	r.mu.Lock()
	r.routeTable = table
	r.mu.Unlock()
	return nil
}

func (r *Router) Routes() []dpproto.Route {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]dpproto.Route, 0, len(r.routeTable))
	for d, n := range r.routeTable {
		out = append(out, dpproto.Route{Dst: d, NextHop: n})
	}
	return out
}

func tunIfIndex(name string) uint32 {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return 0
	}
	return uint32(ifi.Index)
}

func (r *Router) Tuns() []dpproto.TunInfo {
	r.mu.RLock()
	out := make([]dpproto.TunInfo, 0, len(r.tuns))
	for id, t := range r.tuns {
		out = append(out, dpproto.TunInfo{PeerID: id, Name: t.name, IfIndex: tunIfIndex(t.name)})
	}
	r.mu.RUnlock()
	return out
}

func (r *Router) CreateTun(dstPeerID uint32, ifname string) (dpproto.TunInfo, error) {
	pl := r.peerLock(dstPeerID)
	pl.Lock()
	defer pl.Unlock()

	r.mu.RLock()
	t, ok := r.tuns[dstPeerID]
	r.mu.RUnlock()
	if ok {
		if t.name != ifname {
			return dpproto.TunInfo{}, dpproto.Errorf(dpproto.CodeExists, "peerid %d already has tun %q", dstPeerID, t.name)
		}
		return dpproto.TunInfo{PeerID: dstPeerID, Name: t.name, IfIndex: tunIfIndex(t.name)}, nil
	}

	deadline := time.Now().Add(restartGrace)
	for tunIfIndex(ifname) != 0 {
		if time.Now().After(deadline) {
			return dpproto.TunInfo{}, dpproto.Errorf(dpproto.CodeExists, "interface name %q is already in use", ifname)
		}
		time.Sleep(restartPoll)
	}

	d, err := createPeerTun(ifname, dstPeerID, r.dev.mtu)
	if err != nil {
		return dpproto.TunInfo{}, dpproto.Errorf(dpproto.CodeInternal, "creating tun: %v", err)
	}
	name, err := d.Name()
	if err != nil {
		d.Close()
		return dpproto.TunInfo{}, dpproto.Errorf(dpproto.CodeInternal, "naming tun: %v", err)
	}
	w := newTunWriter(dstPeerID)
	r.mu.Lock()
	r.tuns[dstPeerID] = &tunEntry{dev: d, name: name, writer: w}
	r.mu.Unlock()
	go drainTunEvents(d)
	go r.runTunWriter(dstPeerID, d, w)
	r.dev.queue.encryption.wg.Add(1)
	go r.dev.RoutineReadFromTUN(dstPeerID, d)
	return dpproto.TunInfo{PeerID: dstPeerID, Name: name, IfIndex: tunIfIndex(name)}, nil
}

func (r *Router) DestroyTun(dstPeerID uint32) {
	pl := r.peerLock(dstPeerID)
	pl.Lock()
	defer pl.Unlock()

	r.mu.Lock()
	t, ok := r.tuns[dstPeerID]
	if ok {
		delete(r.tuns, dstPeerID)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	if w := t.writer; w != nil {
		close(w.stop)
	drain:
		for {
			select {
			case items := <-w.ch:
				for _, it := range items {
					r.dev.PutMessageBuffer(it.buffer)
				}
			default:
				break drain
			}
		}
	}
	if err := t.dev.Close(); err != nil {
		r.dev.log.Errorf("router: closing tun for peerid %d: %v", dstPeerID, err)
	}
}

func (r *Router) maxTunBatchSize() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	max := 0
	for _, t := range r.tuns {
		if s := t.dev.BatchSize(); s > max {
			max = s
		}
	}
	return max
}

func (r *Router) closeAllTuns() {
	r.mu.Lock()
	tuns := r.tuns
	r.tuns = make(map[uint32]*tunEntry)
	r.mu.Unlock()
	for peerID, t := range tuns {
		if t.writer != nil {
			close(t.writer.stop)
		}
		if err := t.dev.Close(); err != nil {
			r.dev.log.Errorf("closing tun for peerid %d: %v", peerID, err)
		}
	}
}

func (r *Router) resolveNextHop(dstPeerID uint32) *Peer {
	nhid, ok := r.LookupRoute(dstPeerID)
	if !ok {
		return nil
	}
	nextHop := r.dev.lookupPeerByID(nhid)
	if nextHop == nil {
		return nil
	}
	return nextHop
}

func drainTunEvents(dev tun.Device) {
	for range dev.Events() {
	}
}

const (
	restartGrace = 3 * time.Second
	restartPoll  = 50 * time.Millisecond

	headerSize      = 4
	maxIPPacketSize = 65535

	hdrOffTTL = 3

	defaultTTL = 64
)

func createPeerTun(name string, peerID uint32, mtu int) (tun.Device, error) {
	dev, err := tun.CreateTUN(name, mtu)
	if err != nil {
		return nil, err
	}
	return dev, nil
}

const defaultMTU = 1416

const localDeliveryOffset = MessageTransportHeaderSize + headerSize

func writeTunBatched(t tun.Device, items []tunWriteItem) error {
	bufs := make([][]byte, len(items))
	for i, it := range items {
		bufs[i] = it.buffer[:localDeliveryOffset+len(it.packet)-headerSize]
	}
	_, err := t.Write(bufs, localDeliveryOffset)
	return err
}

type tunWriteItem struct {
	buffer *[MaxMessageSize]byte
	packet []byte
}

type tunWriter struct {
	peerID uint32
	ch     chan []tunWriteItem
	stop   chan struct{}
}

const tunWriterQueueSize = 1024

func newTunWriter(peerID uint32) *tunWriter {
	return &tunWriter{
		peerID: peerID,
		ch:     make(chan []tunWriteItem, tunWriterQueueSize),
		stop:   make(chan struct{}),
	}
}

func (r *Router) runTunWriter(peerID uint32, t tun.Device, w *tunWriter) {
	for {
		select {
		case <-w.stop:
			return
		case items := <-w.ch:
			if err := writeTunBatched(t, items); err != nil && !r.dev.isClosed() {
				r.dev.log.Errorf("Failed to write packets to TUN device (peerid %d): %v", peerID, err)
			}
			for _, it := range items {
				r.dev.PutMessageBuffer(it.buffer)
			}
		}
	}
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b [10]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
