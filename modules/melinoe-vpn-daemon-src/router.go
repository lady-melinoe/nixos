package main

import (
	"log"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/tun"
)

// Router owns the TUN devices and the peerid -> nhid route table.
//
// Both are dynamic now: path-vector routing (pathvector.go) discovers
// which peerids are reachable and via which link at runtime, instead of
// this being read once from static [[peer]] config at startup (see
// PROJECT_STATE.md) -- so routeTable/tunByPeerID can be mutated from
// PathVector's goroutines concurrently with reads from the send/receive
// pipeline, hence the mutex this didn't previously need.
//
// This is the melnode-specific replacement for wireguard-go's allowedips
// trie -- see PROJECT_STATE.md's "What replaces the two IP-specific
// blocks". Almost everything that used to live here (batching,
// encrypt/decrypt dispatch, per-peer ordering) is now the ported
// send.go/receive.go pipeline; Router is left with just tun ownership,
// the route table, and the multi-hop re-forward step.
type Router struct {
	dev *Device

	localID   uint32
	tunPrefix string

	// identityPrefix, if set (config: identityPrefix), is assigned as
	// the local address of every tun this node creates for itself --
	// see createAndStartTun. Consistent regardless of which specific
	// link/peer a given tun represents, same idea as a router using one
	// stable loopback/router-id address across every point-to-point
	// interface rather than a different address per link.
	identityPrefix *pvPrefix

	// tunCreateHookBin / tunDestroyHookBin (config: tunCreateHookBin,
	// tunDestroyHookBin), if non-empty, are run as `<bin> <peerid>
	// <ifname>` after a tun is created and configured / after it is
	// closed and deleted. See hooks.go.
	tunCreateHookBin  string
	tunDestroyHookBin string

	mu          sync.RWMutex
	tunByPeerID map[uint32]tun.Device
	tunWriters  map[uint32]*tunWriter // see LookupTunWriter / runTunWriter below
	routeTable  map[uint32]uint32     // dst peerid -> nhid (a link peerid)
}

func newRouter(dev *Device, localID uint32, tunPrefix string, identityPrefix *pvPrefix, tunCreateHookBin, tunDestroyHookBin string) *Router {
	return &Router{
		dev:               dev,
		localID:           localID,
		tunPrefix:         tunPrefix,
		identityPrefix:    identityPrefix,
		tunCreateHookBin:  tunCreateHookBin,
		tunDestroyHookBin: tunDestroyHookBin,
		tunByPeerID:       make(map[uint32]tun.Device),
		tunWriters:        make(map[uint32]*tunWriter),
		routeTable:        make(map[uint32]uint32),
	}
}

// LookupRoute is the RLock'd read used by RoutineReadFromTUN (send.go) to
// resolve a tun's own peerid to its current next-hop link, once per read
// round -- re-resolved every round (not just once at tun-creation time)
// since path-vector can repoint an existing route at a different nhid
// without tearing the tun down.
func (r *Router) LookupRoute(dstPeerID uint32) (nhid uint32, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	nhid, ok = r.routeTable[dstPeerID]
	return
}

// LookupTunWriter is the RLock'd read used by receive.go's local-
// delivery case to find the async tun writer for a given peerid (see
// tunWriter below / PROJECT_STATE.md's backpressure section for why
// this is a writer handle, not the tun.Device itself).
func (r *Router) LookupTunWriter(peerID uint32) (*tunWriter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	w, ok := r.tunWriters[peerID]
	return w, ok
}

// AddOrUpdateRoute installs or repoints a route, creating (and starting
// the reader goroutine for) a tun on first use for this dst -- called
// by PathVector whenever it selects a new best path to some dst, per
// the "create on first route learned" decision (PROJECT_STATE.md).
// Repointing an existing route to a different nhid is just a table
// update; the tun and its reader goroutine are untouched (LookupRoute's
// per-round re-resolution is what makes that safe).
func (r *Router) AddOrUpdateRoute(dstPeerID, nhid uint32) {
	r.mu.Lock()
	_, hadRoute := r.routeTable[dstPeerID]
	r.routeTable[dstPeerID] = nhid
	_, hasTun := r.tunByPeerID[dstPeerID]
	r.mu.Unlock()

	if hadRoute {
		r.dev.log.Verbosef("router: route to peerid %d updated, now via nhid %d", dstPeerID, nhid)
	} else {
		r.dev.log.Verbosef("router: new route to peerid %d via nhid %d", dstPeerID, nhid)
	}

	if !hasTun {
		r.createAndStartTun(dstPeerID)
	}
}

// RemoveRoute withdraws a route and tears down its tun immediately (per
// the "withdraw immediately" decision), closing the tun causes its
// RoutineReadFromTUN goroutine to exit on its next failed Read, same as
// Device.Close() already relies on for shutdown.
func (r *Router) RemoveRoute(dstPeerID uint32) {
	r.mu.Lock()
	delete(r.routeTable, dstPeerID)
	t, hadTun := r.tunByPeerID[dstPeerID]
	if hadTun {
		delete(r.tunByPeerID, dstPeerID)
	}
	w, hadWriter := r.tunWriters[dstPeerID]
	if hadWriter {
		delete(r.tunWriters, dstPeerID)
	}
	r.mu.Unlock()

	if !hadTun {
		return
	}
	if hadWriter {
		close(w.stop)
		// Drain anything already queued rather than leaving it for the
		// writer goroutine, which may already be exiting (racing this
		// close) -- otherwise these buffers never make it back to
		// device.pool.messageBuffers (pools.go). With
		// PreallocatedBuffersPerPool = 0, that pool has no hard cap
		// (Get never blocks), so a leak here is unbounded memory
		// growth over repeated route flaps, not a deadlock -- worth
		// avoiding regardless. (A send that raced in concurrently with
		// this drain, from a lookup that happened just before this
		// function's r.mu.Lock() above, could still slip a batch in
		// after this loop exits -- accepted as a rare, low-impact
		// residual gap, not fully closed here.)
		for {
			select {
			case items := <-w.ch:
				for _, it := range items {
					r.dev.PutMessageBuffer(it.buffer)
				}
			default:
				goto drained
			}
		}
	drained:
	}
	// Grab the name before Close (Name() is unusable afterwards); the
	// destroy hook runs after the interface is gone.
	name, nameErr := t.Name()
	if err := t.Close(); err != nil {
		r.dev.log.Errorf("router: closing tun for no-longer-routable peerid %d: %v", dstPeerID, err)
	}
	if nameErr == nil {
		r.runTunHook("destroy", r.tunDestroyHookBin, dstPeerID, name)
	}
	r.dev.log.Verbosef("router: route to peerid %d withdrawn, tun removed", dstPeerID)
}

// maxTunBatchSize is the RLock'd read used by Device.batchSize().
func (r *Router) maxTunBatchSize() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	max := 0
	for _, t := range r.tunByPeerID {
		if s := t.BatchSize(); s > max {
			max = s
		}
	}
	return max
}

// closeAllTuns closes every currently-open tun -- used by Device.Close()
// for shutdown, as opposed to RemoveRoute's one-at-a-time close when a
// single route is withdrawn during normal operation.
func (r *Router) closeAllTuns() {
	r.mu.Lock()
	tuns := r.tunByPeerID
	r.tunByPeerID = make(map[uint32]tun.Device)
	r.mu.Unlock()
	for peerID, t := range tuns {
		name, nameErr := t.Name()
		if err := t.Close(); err != nil {
			r.dev.log.Errorf("closing tun for peerid %d: %v", peerID, err)
		}
		if nameErr == nil {
			r.runTunHook("destroy", r.tunDestroyHookBin, peerID, name)
		}
	}
}

func (r *Router) createAndStartTun(peerID uint32) {
	t, err := createPeerTun(r.tunPrefix, peerID)
	if err != nil {
		r.dev.log.Errorf("router: failed to create tun for newly-routable peerid %d: %v", peerID, err)
		return
	}

	// Bring the interface administratively up ourselves -- this needs
	// no address (melnode's tuns are pure point-to-point forwarding
	// pipes, see PROJECT_STATE.md), so it doesn't conflict with the
	// address-assignment step createPeerTun's own log line still tells
	// the user to do by hand for anything beyond the identity address
	// below. Without bringing the link up, any netlink route pointing
	// at the tun (kernelroutes.go's InstallPrefixRoute, for prefixes
	// this dest owns) fails outright with ENETDOWN until a human runs
	// `ip link set ... up` on every node -- which would defeat the
	// point of automatic prefix routing entirely. Also assigns this
	// node's own identityPrefix (if configured) as the tun's address --
	// every tun this node owns gets the same identity, not a
	// per-link address, mirroring a router using one stable
	// loopback/router-id across every point-to-point interface. This
	// was originally missing entirely (a real gap, not a deliberate
	// omission -- see PROJECT_STATE.md): the identity prefix was being
	// advertised over path-vector and installed as a kernel route on
	// *other* nodes just fine, but never actually assigned to this
	// node's own tun addresses, so nothing sourced from it actually
	// worked without a human manually running `ip addr add` first.
	if name, err := t.Name(); err == nil {
		if link, err := netlink.LinkByName(name); err == nil {
			if err := netlink.LinkSetUp(link); err != nil {
				r.dev.log.Errorf("router: failed to bring up tun %q: %v", name, err)
			}
			// The node's own identity (config: identityPrefix) goes on
			// every tun it owns, not just one -- e.g. node1 gets
			// 10.99.0.1/32 on both node1-2 and node1-3. `AddrReplace`
			// rather than `AddrAdd`: idempotent if this ever runs twice
			// for some reason, and matches the `ip addr replace`
			// semantics this mirrors.
			if r.identityPrefix != nil {
				addr := &netlink.Addr{IPNet: r.identityPrefix.ipNet()}
				if err := netlink.AddrReplace(link, addr); err != nil {
					r.dev.log.Errorf("router: failed to assign identity %v to tun %q: %v", *r.identityPrefix, name, err)
				} else {
					r.dev.log.Verbosef("router: assigned identity %v to tun %q", *r.identityPrefix, name)
				}
			}
		} else {
			r.dev.log.Errorf("router: netlink lookup for tun %q failed: %v", name, err)
		}
	}

	// Create hook runs once the tun is up and has its identity address,
	// but before traffic can flow (readers/writer not started yet).
	if name, err := t.Name(); err == nil {
		r.runTunHook("create", r.tunCreateHookBin, peerID, name)
	}

	w := newTunWriter(peerID)
	r.mu.Lock()
	r.tunByPeerID[peerID] = t
	r.tunWriters[peerID] = w
	r.mu.Unlock()

	go drainTunEvents(t)
	go r.runTunWriter(peerID, t, w)
	r.dev.queue.encryption.wg.Add(1) // matches RoutineReadFromTUN's own wg.Done() on exit
	go r.dev.RoutineReadFromTUN(peerID, t)
}

// resolveNextHop is the multi-hop re-forward case's routing lookup:
// called from the ported RoutineSequentialReceiver (receive.go) when a
// decrypted transport packet's header `dst` isn't us, to find which
// directly-connected Peer to re-encrypt it onto. Returns nil (having
// already logged why) if there's no route or the resolved link isn't
// live -- the caller just drops the packet in that case.
//
// This -- deciding *where* a decrypted packet should go next -- has no
// wireguard-go equivalent; it's never anyone's job there to re-route a
// packet it just decrypted. What actually staging and sending the
// packet looks like (stealing the inbound element's buffer, batching by
// next hop, StagePackets/SendStagedPackets) lives in receive.go now,
// not here -- an earlier version did one Stage+Send call per forwarded
// packet from inside a per-packet forwardToNextHop, which profiling
// showed serializing what should be one batched send per round (see
// receive.go's comment on forwardsByNextHop).
func (r *Router) resolveNextHop(dstPeerID uint32) *Peer {
	nhid, ok := r.LookupRoute(dstPeerID)
	if !ok {
		log.Printf("router: no route to peerid %d -- dropping forwarded packet", dstPeerID)
		return nil
	}
	nextHop := r.dev.lookupPeerByID(nhid)
	if nextHop == nil {
		log.Printf("router: no such link nhid %d -- dropping forwarded packet", nhid)
		return nil
	}
	return nextHop
}

const (
	headerSize      = 4
	maxIPPacketSize = 65535
)

// createPeerTun creates and returns the TUN device for one discovered
// destination peerid, named "<tunPrefix><peerid>" (e.g. "node-4").
func createPeerTun(namePrefix string, peerID uint32) (tun.Device, error) {
	name := namePrefix + itoa(peerID)
	dev, err := tun.CreateTUN(name, defaultMTU)
	if err != nil {
		return nil, err
	}
	actualName, _ := dev.Name()
	log.Printf("created tun %q for peerid %d (mtu=%d)",
		actualName, peerID, defaultMTU)
	return dev, nil
}

const defaultMTU = 1420

// localDeliveryOffset is how much headroom tun.Device.Write gets to work
// with per buffer (it uses this space itself -- e.g. for a virtio-net
// header when TSO is in play -- it's not available payload space). Our
// decrypted elem.buffer already has exactly this much in front of the
// IP payload for free: MessageTransportHeaderSize (16, wasted -- it held
// the now-consumed transport header) plus headerSize (4, our own
// proto/src/dst/reserved header, also already consumed by this point).
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
