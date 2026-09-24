package main

import (
	"fmt"
	"sync"

	"golang.zx2c4.com/wireguard/tun"

	"melnode/dpproto"
)

// Router owns the tun devices and the dst-peerid -> next-hop-link table: the
// data plane's entire forwarding state.
//
// Both are programmed by the control plane (ctl.go), which discovers
// reachability with path-vector routing and pushes the result down here --
// this side never decides anything, it just looks up. That makes the maps
// mutable from the control-socket goroutine concurrently with reads from the
// send/receive pipeline, hence the mutex.
//
// This is the melnode-specific replacement for wireguard-go's allowedips
// trie. Almost everything that used to live here (batching, encrypt/decrypt
// dispatch, per-peer ordering) is the ported send.go/receive.go pipeline;
// Router is left with tun ownership, the route table, and the multi-hop
// re-forward step.
type Router struct {
	dev *Device

	localID uint32

	// peerLocks[peerid] serializes that peer's tun lifecycle (create, start,
	// destroy), so two racing requests for the same peerid can't both create
	// it (the loser failing with EBUSY) or start it twice. Guarded by mu; the
	// mutexes themselves are never removed (peerids are 0-255). Lock order: a
	// peer lock, then mu -- never the other way round.
	peerLocks map[uint32]*sync.Mutex

	mu         sync.RWMutex
	tuns       map[uint32]*tunEntry
	routeTable map[uint32]uint32 // dst peerid -> nhid (a link peerid)
}

// tunEntry is one destination's tun. A tun is created first and started later
// (StartTun): until then nothing reads from it or delivers into it, which
// gives the control plane a window to configure the interface -- bring it up,
// address it, run its hooks -- before any traffic can flow.
type tunEntry struct {
	dev     tun.Device
	name    string
	started bool
	writer  *tunWriter // set once started, see runTunWriter
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

// peerLock returns the lifecycle lock for one peerid, see Router.peerLocks.
// Must not be called with r.mu held.
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

// LookupRoute is the RLock'd read used by RoutineReadFromTUN (send.go) to
// resolve a tun's own peerid to its current next-hop link, once per read
// round -- re-resolved every round (not just once at tun-creation time)
// since the control plane can repoint an existing route at a different nhid
// without tearing the tun down.
func (r *Router) LookupRoute(dstPeerID uint32) (nhid uint32, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	nhid, ok = r.routeTable[dstPeerID]
	return
}

// LookupTunWriter is the RLock'd read used by receive.go's local-
// delivery case to find the async tun writer for a given peerid (see
// tunWriter below for why this is a writer handle, not the tun.Device
// itself). Only started tuns have one.
func (r *Router) LookupTunWriter(peerID uint32) (*tunWriter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tuns[peerID]
	if !ok || t.writer == nil {
		return nil, false
	}
	return t.writer, true
}

// SetRoute installs or changes the next hop for dst. Takes effect on the very
// next packet.
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

// DelRoute removes dst's route (no-op if absent).
func (r *Router) DelRoute(dst uint32) {
	r.mu.Lock()
	_, had := r.routeTable[dst]
	delete(r.routeTable, dst)
	r.mu.Unlock()
	if had {
	}
}

// Routes returns a snapshot of the route table.
func (r *Router) Routes() []dpproto.Route {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]dpproto.Route, 0, len(r.routeTable))
	for d, n := range r.routeTable {
		out = append(out, dpproto.Route{Dst: d, NextHop: n})
	}
	return out
}

// Tuns returns a snapshot of the tuns.
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

// CreateTun creates dstPeerID's tun if it doesn't have one, WITHOUT starting
// it (see tunEntry). Idempotent: an existing tun's name and state are
// returned unchanged.
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

// StartTun begins moving traffic through a created tun: it starts the async
// writer (packets arriving for this node from dstPeerID) and the reader
// goroutine (packets the host sends into the tun). Idempotent.
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
	r.dev.queue.encryption.wg.Add(1) // matches RoutineReadFromTUN's own wg.Done() on exit
	go r.dev.RoutineReadFromTUN(dstPeerID, t.dev)
	return nil
}

// DestroyTun tears down dstPeerID's tun immediately: closing it makes its
// RoutineReadFromTUN goroutine exit on its next failed Read, same as
// Device.Close() already relies on for shutdown. Idempotent.
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
		// Drain anything already queued rather than leaving it for the
		// writer goroutine, which may already be exiting (racing this
		// close) -- otherwise these buffers never make it back to
		// device.pool.messageBuffers (pools.go). With
		// PreallocatedBuffersPerPool = 0, that pool has no hard cap
		// (Get never blocks), so a leak here is unbounded memory
		// growth over repeated route flaps, not a deadlock -- worth
		// avoiding regardless. (A send that raced in concurrently with
		// this drain, from a lookup that happened just before the
		// delete above, could still slip a batch in after this loop
		// exits -- accepted as a rare, low-impact residual gap.)
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

// maxTunBatchSize is the RLock'd read used by Device.batchSize().
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

// closeAllTuns closes every currently-open tun -- used by Device.Close()
// for shutdown.
func (r *Router) closeAllTuns() {
	r.mu.Lock()
	tuns := r.tuns
	r.tuns = make(map[uint32]*tunEntry)
	r.mu.Unlock()
	for peerID, t := range tuns {
		if err := t.dev.Close(); err != nil {
			r.dev.log.Errorf("closing tun for peerid %d: %v", peerID, err)
		}
	}
}

// resolveNextHop is the multi-hop re-forward case's routing lookup:
// called from the ported RoutineSequentialReceiver (receive.go) when a
// decrypted transport packet's header `dst` isn't us, to find which
// directly-connected Peer to re-encrypt it onto. Returns nil (having
// already logged why) if there's no route or the resolved link isn't
// live -- the caller just drops the packet in that case.
//
// What actually staging and sending the packet looks like (stealing the
// inbound element's buffer, batching by next hop, StagePackets/
// SendStagedPackets) lives in receive.go, not here -- see its comment on
// forwardsByNextHop.
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

// drainTunEvents drains a TUN device's Events() channel so its internal
// listener never blocks; we don't act on up/down/MTU events (melnode has N
// tuns, one per currently-routable peerid, not one tun whose up/down state
// drives the whole device's).
func drainTunEvents(dev tun.Device) {
	for range dev.Events() {
	}
}

const (
	headerSize      = 4
	maxIPPacketSize = 65535

	// Routing header layout (headerSize bytes, in front of every payload):
	//   0: proto -- 0 is a tunneled IP packet, handled entirely here; any
	//      other value is a control packet, punted to the control plane
	//      (see receive.go and ctl.go) which owns what each proto means
	//   1: src peerid
	//   2: dst peerid
	//   3: ttl -- decremented by each forwarding hop; a packet that would
	//      reach 0 is dropped instead of forwarded (IP-style: the final
	//      destination delivers regardless of ttl, only forwarding checks it).
	hdrOffTTL = 3

	// defaultTTL is stamped on every freshly originated proto=0 packet.
	// Comfortably above any sane mesh diameter (see pvMaxPathLen's note),
	// low enough that a transient routing loop burns out quickly.
	defaultTTL = 64
)

// createPeerTun creates and returns the TUN device for one destination
// peerid, with the interface name the control plane chose.
func createPeerTun(name string, peerID uint32, mtu int) (tun.Device, error) {
	dev, err := tun.CreateTUN(name, mtu)
	if err != nil {
		return nil, err
	}
	return dev, nil
}

// defaultMTU is the default tun MTU (config: mtu). WireGuard's 1420 is
// 1500 minus IPv6 (40) + UDP (8) + WireGuard's transport overhead (16-byte
// header + 16-byte tag); melnode adds its own 4-byte routing header inside
// the encrypted payload, so the same 1500-byte underlay leaves 1416.
const defaultMTU = 1416

// localDeliveryOffset is how much headroom tun.Device.Write gets to work
// with per buffer (it uses this space itself -- e.g. for a virtio-net
// header when TSO is in play -- it's not available payload space). Our
// decrypted elem.buffer already has exactly this much in front of the
// IP payload for free: MessageTransportHeaderSize (16, wasted -- it held
// the now-consumed transport header) plus headerSize (4, our own
// proto/src/dst/ttl header, also already consumed by this point).
// That's what makes writeTunBatched below zero-copy: elem.buffer doesn't
// need to be reshaped at all, just resliced.
const localDeliveryOffset = MessageTransportHeaderSize + headerSize

// writeTunBatched delivers decrypted, header-stripped IP payloads to one
// local tun, reusing each QueueInboundElement's existing elem.buffer
// directly (see localDeliveryOffset) rather than allocating and copying
// a fresh buffer per packet -- an earlier version did the latter, which
// profiling on real hardware showed costing 18-30% of total CPU time
// under TCP load (see PROJECT_STATE.md's optimization notes). Elements
// are still owned by the caller (receive.go's RoutineSequentialReceiver)
// and returned to the pool there, after this call returns.
func writeTunBatched(t tun.Device, items []tunWriteItem) error {
	bufs := make([][]byte, len(items))
	for i, it := range items {
		bufs[i] = it.buffer[:localDeliveryOffset+len(it.packet)-headerSize]
	}
	_, err := t.Write(bufs, localDeliveryOffset)
	return err
}

// tunWriteItem is the small, ownership-transferred slice of a
// QueueInboundElement that the async tun writer actually needs --
// deliberately not the whole *QueueInboundElement, since that wrapper
// gets recycled back to its own pool immediately by
// RoutineSequentialReceiver's per-round cleanup (see receive.go), while
// the buffer this points at is what's actually still in flight to the
// writer goroutine below.
type tunWriteItem struct {
	buffer *[MaxMessageSize]byte
	packet []byte
}

// tunWriter decouples a tun's actual Write(2) syscall from whichever
// peer's RoutineSequentialReceiver is delivering to it locally, so a
// slow local consumer (nothing reading the other end of this tun)
// can't block that receiver's *next* round -- which, left blocked,
// would delay that peer's own future liveness/path-vector packet
// handling too, not just further local deliveries. See
// PROJECT_STATE.md's backpressure section (this is the "tun outbound"
// half of it; submit() in send.go is the "forwarded packets'
// re-encryption and UDP outbound" half).
type tunWriter struct {
	peerID uint32
	ch     chan []tunWriteItem
	stop   chan struct{}
}

// tunWriterQueueSize bounds how many rounds' worth of local-delivery
// traffic can queue up behind the tun-writer goroutine before newer
// rounds start getting tail-dropped. Originally set to 8 ("deliberately
// small") on the theory that this only needs to smooth over a slow
// consumer briefly -- that was wrong: it made melnode itself the
// bottleneck for any real sustained local-delivery throughput, well
// below what the actual link/CPU could otherwise sustain (measured:
// UDP at 50Mbit/s through a two-node test link saw 8% loss at
// tunWriterQueueSize=8, and 0% at this value -- see PROJECT_STATE.md).
// Matches QueueOutboundSize/QueueInboundSize/QueueHandshakeSize's own
// established 1024 rather than inventing a smaller number a second
// time.
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
