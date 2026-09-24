package main

import (
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

// Peer is one [[link]] entry: a Noise IK(psk2, with a zero/unconfigured
// psk) tunnel to a directly-connected node. Multi-hop peers (a [[peer]]
// entry with nhid pointing elsewhere) are not a Peer here -- they're
// purely a routing-table entry in router.go pointing at one of these.
//
// Field shape is deliberately kept identical to wireguard-go's own
// device/peer.go Peer struct (queue/timers/endpoint/keypairs/handshake),
// per PROJECT_STATE.md, since the ported send.go/receive.go/timers.go/
// noise-protocol.go all reach into these fields directly. Dropped
// relative to upstream: trieEntries (allowedips bookkeeping -- melnode
// has no allowedips trie). Kept: cookieGenerator/persistentKeepaliveInterval,
// even though melnode's config never sets a persistent keepalive interval
// (it just stays 0, i.e. disabled, which the ported timers.go already
// treats as "off").
type Peer struct {
	isRunning         atomic.Bool
	keypairs          Keypairs
	handshake         Handshake
	device            *Device
	stopping          sync.WaitGroup // routines pending stop
	txBytes           atomic.Uint64
	rxBytes           atomic.Uint64
	lastHandshakeNano atomic.Int64

	id uint32 // the link's peerid (melnode addition -- wireguard-go has no equivalent, identifies peers by pubkey only)

	endpoint struct {
		sync.Mutex
		val            conn.Endpoint
		clearSrcOnTx   bool
		disableRoaming bool
	}

	timers struct {
		retransmitHandshake     *Timer
		sendKeepalive           *Timer
		newHandshake            *Timer
		zeroKeyMaterial         *Timer
		persistentKeepalive     *Timer
		handshakeAttempts       atomic.Uint32
		needAnotherKeepalive    atomic.Bool
		sentLastMinuteHandshake atomic.Bool
	}

	state struct {
		sync.Mutex
	}

	queue struct {
		staged          chan *QueueOutboundElementsContainer
		stagedControl   chan *QueueOutboundElementsContainer // separate from staged -- see StagePackets (send.go) / PROJECT_STATE.md's backpressure section
		outbound        *autodrainingOutboundQueue
		controlOutbound *autodrainingOutboundQueue
		inbound         *autodrainingInboundQueue
	}

	cookieGenerator             CookieGenerator
	persistentKeepaliveInterval atomic.Uint32
}

// newPeer constructs a Peer for one [[link]] entry. Unlike wireguard-go's
// UAPI-driven NewPeer (which takes a lock, checks MaxPeers, and mutates
// device.peers.keyMap itself), melnode's peers are all known upfront from
// config, so construction and registration (Device.addPeer, in device.go)
// are separate steps, same as melnode did before this port.
func newPeer(dev *Device, id uint32, remoteStatic NoisePublicKey, endpoint conn.Endpoint) (*Peer, error) {
	peer := &Peer{
		device: dev,
		id:     id,
	}
	peer.cookieGenerator.Init(remoteStatic)
	peer.queue.outbound = newAutodrainingOutboundQueue(dev)
	peer.queue.controlOutbound = newAutodrainingOutboundQueue(dev)
	peer.queue.inbound = newAutodrainingInboundQueue(dev)
	peer.queue.staged = make(chan *QueueOutboundElementsContainer, QueueStagedSize)
	peer.queue.stagedControl = make(chan *QueueOutboundElementsContainer, QueueStagedSize)

	handshake := &peer.handshake
	handshake.mutex.Lock()
	handshake.precomputedStaticStatic, _ = dev.staticIdentity.privateKey.sharedSecret(remoteStatic)
	handshake.remoteStatic = remoteStatic
	handshake.mutex.Unlock()

	peer.endpoint.Lock()
	peer.endpoint.val = endpoint
	peer.endpoint.Unlock()

	peer.timersInit()

	return peer, nil
}

func (peer *Peer) SendBuffers(buffers [][]byte) error {
	peer.device.net.RLock()
	defer peer.device.net.RUnlock()

	if peer.device.isClosed() {
		return nil
	}

	peer.endpoint.Lock()
	endpoint := peer.endpoint.val
	if endpoint == nil {
		peer.endpoint.Unlock()
		return errNoKnownEndpoint
	}
	if peer.endpoint.clearSrcOnTx {
		endpoint.ClearSrc()
		peer.endpoint.clearSrcOnTx = false
	}
	peer.endpoint.Unlock()

	err := peer.device.net.bind.Send(buffers, endpoint)
	if err == nil {
		var totalLen uint64
		for _, b := range buffers {
			totalLen += uint64(len(b))
		}
		peer.txBytes.Add(totalLen)
	}
	return err
}

func (peer *Peer) String() string {
	return "peer(" + itoa(peer.id) + ")"
}

func (peer *Peer) Start() {
	if peer.device.isClosed() {
		return
	}

	peer.state.Lock()
	defer peer.state.Unlock()

	if peer.isRunning.Load() {
		return
	}

	device := peer.device

	peer.stopping.Wait()
	peer.stopping.Add(2)

	peer.handshake.mutex.Lock()
	peer.handshake.lastSentHandshake = time.Now().Add(-(RekeyTimeout + time.Second))
	peer.handshake.mutex.Unlock()

	peer.device.queue.encryption.wg.Add(1)
	peer.device.queue.controlEncryption.wg.Add(1)

	peer.timersStart()

	device.flushInboundQueue(peer.queue.inbound)
	device.flushOutboundQueue(peer.queue.outbound)
	device.flushOutboundQueue(peer.queue.controlOutbound)

	batchSize := peer.device.BatchSize()
	go peer.RoutineSequentialSender(batchSize)
	go peer.RoutineSequentialReceiver(batchSize)

	peer.isRunning.Store(true)

}

func (peer *Peer) ZeroAndFlushAll() {
	device := peer.device

	keypairs := &peer.keypairs
	keypairs.Lock()
	device.DeleteKeypair(keypairs.previous)
	device.DeleteKeypair(keypairs.current)
	device.DeleteKeypair(keypairs.next.Load())
	keypairs.previous = nil
	keypairs.current = nil
	keypairs.next.Store(nil)
	keypairs.Unlock()

	handshake := &peer.handshake
	handshake.mutex.Lock()
	device.indexTable.Delete(handshake.localIndex)
	handshake.Clear()
	handshake.mutex.Unlock()

	peer.FlushStagedPackets()
}

func (peer *Peer) ExpireCurrentKeypairs() {
	handshake := &peer.handshake
	handshake.mutex.Lock()
	peer.device.indexTable.Delete(handshake.localIndex)
	handshake.Clear()
	peer.handshake.lastSentHandshake = time.Now().Add(-(RekeyTimeout + time.Second))
	handshake.mutex.Unlock()

	keypairs := &peer.keypairs
	keypairs.Lock()
	if keypairs.current != nil {
		keypairs.current.sendNonce.Store(RejectAfterMessages)
	}
	if next := keypairs.next.Load(); next != nil {
		next.sendNonce.Store(RejectAfterMessages)
	}
	keypairs.Unlock()
}

func (peer *Peer) Stop() {
	peer.state.Lock()
	defer peer.state.Unlock()

	if !peer.isRunning.Swap(false) {
		return
	}

	peer.timersStop()
	peer.queue.inbound.c <- nil
	peer.queue.outbound.c <- nil
	peer.queue.controlOutbound.c <- nil
	peer.stopping.Wait()
	peer.device.queue.encryption.wg.Done()
	peer.device.queue.controlEncryption.wg.Done()

	peer.ZeroAndFlushAll()
}

func (peer *Peer) SetEndpointFromPacket(endpoint conn.Endpoint) {
	peer.endpoint.Lock()
	defer peer.endpoint.Unlock()
	if peer.endpoint.disableRoaming {
		return
	}
	peer.endpoint.clearSrcOnTx = false
	peer.endpoint.val = endpoint
}

func (peer *Peer) markEndpointSrcForClearing() {
	peer.endpoint.Lock()
	defer peer.endpoint.Unlock()
	if peer.endpoint.val == nil {
		return
	}
	peer.endpoint.clearSrcOnTx = true
}
