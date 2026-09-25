package main

import (
	"fmt"
	"sync"

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
	dev     tun.Device
	name    string
	started bool
	writer  *tunWriter
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

func (r *Router) SetRoute(dst, nh uint32) {
	r.mu.Lock()
	old, had := r.routeTable[dst]
	r.routeTable[dst] = nh
	r.mu.Unlock()
	switch {
	case !had:
	case old != nh:
	}
}

func (r *Router) DelRoute(dst uint32) {
	r.mu.Lock()
	_, had := r.routeTable[dst]
	delete(r.routeTable, dst)
	r.mu.Unlock()
	if had {
	}
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

func (r *Router) Tuns() []dpproto.TunInfo {
	r.mu.RLock()
	entries := make(map[uint32]*tunEntry, len(r.tuns))
	started := make(map[uint32]bool, len(r.tuns))
	for id, t := range r.tuns {
		entries[id] = t
		started[id] = t.started
	}
	r.mu.RUnlock()
	out := make([]dpproto.TunInfo, 0, len(entries))
	for id, t := range entries {
		mtu, _ := t.dev.MTU()
		out = append(out, dpproto.TunInfo{PeerID: id, Name: t.name, MTU: uint32(mtu), Started: started[id]})
	}
	return out
}

func (r *Router) CreateTun(dstPeerID uint32, ifname string) (name string, started bool, err error) {
	pl := r.peerLock(dstPeerID)
	pl.Lock()
	defer pl.Unlock()

	r.mu.RLock()
	t, ok := r.tuns[dstPeerID]
	if ok {
		name, started = t.name, t.started
	}
	r.mu.RUnlock()
	if ok {
		return name, started, nil
	}

	d, err := createPeerTun(ifname, dstPeerID, r.dev.mtu)
	if err != nil {
		return "", false, fmt.Errorf("creating tun: %w", err)
	}
	name, err = d.Name()
	if err != nil {
		d.Close()
		return "", false, fmt.Errorf("naming tun: %w", err)
	}
	r.mu.Lock()
	r.tuns[dstPeerID] = &tunEntry{dev: d, name: name}
	r.mu.Unlock()
	go drainTunEvents(d)
	return name, false, nil
}

func (r *Router) StartTun(dstPeerID uint32) error {
	pl := r.peerLock(dstPeerID)
	pl.Lock()
	defer pl.Unlock()

	r.mu.Lock()
	t, ok := r.tuns[dstPeerID]
	if !ok {
		r.mu.Unlock()
		return dpproto.Errorf(dpproto.CodeNotFound, "no tun for peerid %d", dstPeerID)
	}
	if t.started {
		r.mu.Unlock()
		return nil
	}
	w := newTunWriter(dstPeerID)
	t.writer = w
	t.started = true
	r.mu.Unlock()

	go r.runTunWriter(dstPeerID, t.dev, w)
	r.dev.queue.encryption.wg.Add(1)
	go r.dev.RoutineReadFromTUN(dstPeerID, t.dev)
	return nil
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
