/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package main

import (
	"encoding/binary"
	"errors"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/tun"
)

/* Outbound flow
 *
 * 1. TUN queue
 * 2. Routing (sequential)
 * 3. Nonce assignment (sequential)
 * 4. Encryption (parallel)
 * 5. Transmission (sequential)
 *
 * The functions in this file occur (roughly) in the order in
 * which the packets are processed.
 *
 * Locking, Producers and Consumers
 *
 * The order of packets (per peer) must be maintained,
 * but encryption of packets happen out-of-order:
 *
 * The sequential consumers will attempt to take the lock,
 * workers release lock when they have completed work (encryption) on the packet.
 *
 * If the element is inserted into the "encryption queue",
 * the content is preceded by enough "junk" to contain the transport header
 * (to allow the construction of transport messages in-place)
 */

type QueueOutboundElement struct {
	buffer  *[MaxMessageSize]byte // slice holding the packet data
	packet  []byte                // slice of "buffer" (always!)
	nonce   uint64                // nonce for encryption
	keypair *Keypair              // keypair for encryption
	peer    *Peer                 // related peer
}

type QueueOutboundElementsContainer struct {
	sync.Mutex
	elems     []*QueueOutboundElement
	isControl bool // liveness (proto=1) / path-vector (proto=2) vs routed data (proto=0) -- see StagePackets/drainStaged and PROJECT_STATE.md's backpressure section
}

func (device *Device) NewOutboundElement() *QueueOutboundElement {
	elem := device.GetOutboundElement()
	elem.buffer = device.GetMessageBuffer()
	elem.nonce = 0
	// keypair and peer were cleared (if necessary) by clearPointers.
	return elem
}

// clearPointers clears elem fields that contain pointers.
// This makes the garbage collector's life easier and
// avoids accidentally keeping other objects around unnecessarily.
// It also reduces the possible collateral damage from use-after-free bugs.
func (elem *QueueOutboundElement) clearPointers() {
	elem.buffer = nil
	elem.packet = nil
	elem.keypair = nil
	elem.peer = nil
}

/* Queues a keepalive if no packets are queued for peer
 */
func (peer *Peer) SendKeepalive() {
	if len(peer.queue.staged) == 0 && peer.isRunning.Load() {
		elem := peer.device.NewOutboundElement()
		elemsContainer := peer.device.GetOutboundElementsContainer()
		elemsContainer.elems = append(elemsContainer.elems, elem)
		select {
		case peer.queue.staged <- elemsContainer:
			peer.device.log.Verbosef("%v - Sending keepalive packet", peer)
		default:
			peer.device.PutMessageBuffer(elem.buffer)
			peer.device.PutOutboundElement(elem)
			peer.device.PutOutboundElementsContainer(elemsContainer)
		}
	}
	peer.SendStagedPackets()
}

func (peer *Peer) SendHandshakeInitiation(isRetry bool) error {
	if !isRetry {
		peer.timers.handshakeAttempts.Store(0)
	}

	peer.handshake.mutex.RLock()
	if time.Since(peer.handshake.lastSentHandshake) < RekeyTimeout {
		peer.handshake.mutex.RUnlock()
		return nil
	}
	peer.handshake.mutex.RUnlock()

	peer.handshake.mutex.Lock()
	if time.Since(peer.handshake.lastSentHandshake) < RekeyTimeout {
		peer.handshake.mutex.Unlock()
		return nil
	}
	peer.handshake.lastSentHandshake = time.Now()
	peer.handshake.mutex.Unlock()

	peer.device.log.Verbosef("%v - Sending handshake initiation", peer)

	msg, err := peer.device.CreateMessageInitiation(peer)
	if err != nil {
		peer.device.log.Errorf("%v - Failed to create initiation message: %v", peer, err)
		return err
	}

	packet := make([]byte, MessageInitiationSize)
	_ = msg.marshal(packet)
	peer.cookieGenerator.AddMacs(packet)

	peer.timersAnyAuthenticatedPacketTraversal()
	peer.timersAnyAuthenticatedPacketSent()

	err = peer.SendBuffers([][]byte{packet})
	if err != nil {
		peer.device.log.Errorf("%v - Failed to send handshake initiation: %v", peer, err)
	}
	peer.timersHandshakeInitiated()

	return err
}

func (peer *Peer) SendHandshakeResponse() error {
	peer.handshake.mutex.Lock()
	peer.handshake.lastSentHandshake = time.Now()
	peer.handshake.mutex.Unlock()

	peer.device.log.Verbosef("%v - Sending handshake response", peer)

	response, err := peer.device.CreateMessageResponse(peer)
	if err != nil {
		peer.device.log.Errorf("%v - Failed to create response message: %v", peer, err)
		return err
	}

	packet := make([]byte, MessageResponseSize)
	_ = response.marshal(packet)
	peer.cookieGenerator.AddMacs(packet)

	err = peer.BeginSymmetricSession()
	if err != nil {
		peer.device.log.Errorf("%v - Failed to derive keypair: %v", peer, err)
		return err
	}

	peer.timersSessionDerived()
	peer.timersAnyAuthenticatedPacketTraversal()
	peer.timersAnyAuthenticatedPacketSent()

	// TODO: allocation could be avoided
	err = peer.SendBuffers([][]byte{packet})
	if err != nil {
		peer.device.log.Errorf("%v - Failed to send handshake response: %v", peer, err)
	}
	return err
}

func (device *Device) SendHandshakeCookie(initiatingElem *QueueHandshakeElement) error {
	device.log.Verbosef("Sending cookie response for denied handshake message for %v", initiatingElem.endpoint.DstToString())

	sender := binary.LittleEndian.Uint32(initiatingElem.packet[4:8])
	reply, err := device.cookieChecker.CreateReply(initiatingElem.packet, sender, initiatingElem.endpoint.DstToBytes())
	if err != nil {
		device.log.Errorf("Failed to create cookie reply: %v", err)
		return err
	}

	packet := make([]byte, MessageCookieReplySize)
	_ = reply.marshal(packet)
	// TODO: allocation could be avoided
	device.net.bind.Send([][]byte{packet}, initiatingElem.endpoint)

	return nil
}

func (peer *Peer) keepKeyFreshSending() {
	keypair := peer.keypairs.Current()
	if keypair == nil {
		return
	}
	nonce := keypair.sendNonce.Load()
	if nonce > RekeyAfterMessages || (keypair.isInitiator && time.Since(keypair.created) > RekeyAfterTime) {
		peer.SendHandshakeInitiation(false)
	}
}

// RoutineReadFromTUN is the read-side goroutine for one peer's TUN
// interface -- melnode has N tuns (one per [[peer]] entry), so unlike
// wireguard-go (exactly one device-wide tun), this is parameterized per
// peer-tun and started once per [[peer]] from main.go, same as
// router.go's old runTunReader did.
//
// The IP-awareness wireguard-go had here (an allowedips.Lookup keyed off
// the tunneled IP packet's destination address) is replaced with
// melnode's own header + routeTable: peerID is this tun's own peerid
// (the packet's ultimate destination, per melnode's header semantics --
// see PROJECT_STATE.md), so the only thing needed per read-round is
// resolving peerID's next hop and stamping our header on.
func (device *Device) RoutineReadFromTUN(peerID uint32, devTun tun.Device) {
	defer func() {
		device.log.Verbosef("Routine: TUN reader (peerid %d) - stopped", peerID)
		device.queue.encryption.wg.Done()
	}()

	device.log.Verbosef("Routine: TUN reader (peerid %d) - started", peerID)

	var (
		batchSize = devTun.BatchSize()
		readErr   error
		elems     = make([]*QueueOutboundElement, batchSize)
		bufs      = make([][]byte, batchSize)
		count     = 0
		sizes     = make([]int, batchSize)
		// Leave room for both the transport header (stamped later by
		// RoutineEncryption) and our own 4-byte routing header in front
		// of the tunneled IP packet.
		offset = MessageTransportHeaderSize + headerSize
	)

	for i := range elems {
		elems[i] = device.NewOutboundElement()
		bufs[i] = elems[i].buffer[:]
	}

	defer func() {
		for _, elem := range elems {
			if elem != nil {
				device.PutMessageBuffer(elem.buffer)
				device.PutOutboundElement(elem)
			}
		}
	}()

	for {
		// read packets
		count, readErr = devTun.Read(bufs, sizes, offset)
		if count > 0 {
			elemsForPeer := device.GetOutboundElementsContainer()
			for i := 0; i < count; i++ {
				if sizes[i] < 1 {
					continue
				}

				elem := elems[i]
				buf := elem.buffer[:]

				// Stamp our own 4-byte routing header (proto=0, our
				// header semantics -- see PROJECT_STATE.md) right
				// in front of the tunneled IP packet wireguard-go's own
				// device.tun.device.Read just wrote at `offset`. src is
				// always us; dst is this tun's own peerid -- this is the
				// only place a fresh packet's header is ever stamped
				// (intermediate hops just forward it unchanged, see
				// router.go's forwardToNextHop).
				buf[offset-headerSize+0] = 0 // proto 0: routed IP packet
				buf[offset-headerSize+1] = byte(device.localID)
				buf[offset-headerSize+2] = byte(peerID)
				buf[offset-headerSize+3] = 0
				elem.packet = buf[offset-headerSize : offset+sizes[i]]

				elemsForPeer.elems = append(elemsForPeer.elems, elem)
				elems[i] = device.NewOutboundElement()
				bufs[i] = elems[i].buffer[:]
			}

			if len(elemsForPeer.elems) == 0 {
				device.PutOutboundElementsContainer(elemsForPeer)
			} else if peer := device.router.resolveNextHop(peerID); peer != nil && peer.isRunning.Load() {
				// Re-resolved every round (not cached once at startup,
				// the way this used to work with a static route table)
				// so a path-vector reroute of this dst to a different
				// nhid takes effect on this tun's very next read round,
				// without needing to tear the tun down -- see
				// router.go's LookupRoute/AddOrUpdateRoute comments.
				peer.StagePackets(elemsForPeer)
				peer.SendStagedPackets()
			} else {
				for _, elem := range elemsForPeer.elems {
					device.PutMessageBuffer(elem.buffer)
					device.PutOutboundElement(elem)
				}
				device.PutOutboundElementsContainer(elemsForPeer)
			}
		}

		if readErr != nil {
			if errors.Is(readErr, tun.ErrTooManySegments) {
				// TODO: record stat for this
				// This will happen if MSS is surprisingly small (< 576)
				// coincident with reasonably high throughput.
				device.log.Verbosef("Dropped some packets from multi-segment read: %v", readErr)
				continue
			}
			if !device.isClosed() {
				if !errors.Is(readErr, os.ErrClosed) {
					device.log.Errorf("Failed to read packet from TUN device: %v", readErr)
				}
			}
			return
		}
	}
}

// StagePackets enqueues one round's worth of outbound packets, routing
// control (liveness/path-vector, elems.isControl) and routed data into
// entirely separate per-peer channels -- this separation, kept all the
// way through to submit()/RoutineSequentialSender below, is what
// actually gives control traffic priority: it's never sitting in the
// same FIFO behind a backlog of data, so a full data queue can't delay
// it structurally, regardless of how congested data gets.
//
// When a queue is full, the newest batch is tail-dropped (an earlier
// version of this instead evicted the *oldest* already-staged batch --
// see PROJECT_STATE.md's backpressure section for why this changed:
// tail-drop is the standard behavior for a congested queue in both
// typical network hardware and Linux's own default qdisc, and is what
// was asked for here).
func (peer *Peer) StagePackets(elems *QueueOutboundElementsContainer) {
	ch := peer.queue.staged
	class := "data"
	if elems.isControl {
		ch = peer.queue.stagedControl
		class = "control"
	}

	select {
	case ch <- elems:
		return
	default:
	}

	n := len(elems.elems)
	for _, elem := range elems.elems {
		peer.device.PutMessageBuffer(elem.buffer)
		peer.device.PutOutboundElement(elem)
	}
	peer.device.PutOutboundElementsContainer(elems)
	peer.device.log.Verbosef("%v - staged %s queue full, dropping newest batch (%d packets)", peer, class, n)
}

// submit hands a container off to this peer's ordered UDP-send queue
// and to the encryption workers, using the control or data pair of
// queues per elemsContainer's class (peer.queue.controlOutbound /
// device.queue.controlEncryption vs their data equivalents -- kept
// fully separate end-to-end, see StagePackets above).
//
// Only the outbound-queue send is non-blocking (tail-drop if full).
// This is the fix for the cross-peer stall PROJECT_STATE.md describes:
// without it, a slow/congested peer's own outbound queue filling up
// could block whichever goroutine is submitting to it -- and for a
// forwarded packet, that's a *different* peer's own
// RoutineSequentialReceiver (receive.go's forwardsByNextHop), so a
// congested next hop could stall a totally unrelated peer's own
// control-packet processing.
//
// The encryption-queue send deliberately stays a blocking send, same
// as before this change: it's a CPU-bound resource actively drained by
// every encryption worker device-wide (RoutineEncryption), not a
// specific peer's network link, so it's a much less likely bottleneck
// for the scenario this exists to fix. Making it non-blocking too would
// need real cross-channel atomicity -- device.queue.encryption.c and
// peer.queue.outbound.c only work as the matched pair
// RoutineSequentialSender expects if both sends always succeed
// together; dropping one independently after the other already
// succeeded either leaks a container forever-Locked in whichever queue
// got it, or leaves RoutineSequentialSender permanently blocked
// waiting on an Unlock that will never come. Not attempted here.
// submit hands a container off to this peer's ordered UDP-send queue
// and to the (shared, device-wide) encryption workers, atomically: both
// sends succeed or neither does, checked-and-committed under one of
// two device-wide mutexes (one per class -- see device.queue's own
// doc comment). If either queue is at capacity, the whole batch is
// tail-dropped (per PROJECT_STATE.md's backpressure section) rather
// than blocking the caller on either queue individually.
//
// This used to leave the encryption-queue send as a plain blocking
// channel send, on the theory that a CPU-bound resource actively
// drained by every encryption worker device-wide was a much less
// likely bottleneck than a specific peer's network link -- and that
// avoiding an atomic two-channel commit (which needs this
// device-wide serialization) was worth it. That reasoning turned out
// to be wrong under real sustained multi-Gbit/s load (see
// PROJECT_STATE.md): when encryption genuinely can't keep pace, even
// briefly, that shared queue backs up toward its full capacity, and
// since each queued container can hold many packets' worth of
// MaxMessageSize (64KB) buffers, a queue that's merely "backed up, not
// yet full" already means multiple GB of entirely legitimate, still-
// referenced data in flight -- not a leak, but enough to force Go's GC
// to keep raising its heap goal, and (more importantly for the
// reported symptom) enough for the blocking send itself to stall
// whichever goroutine was submitting, which for RoutineReadFromTUN
// means no further tun.Read() calls until room appears. Tail-dropping
// here instead means a real, momentary encryption bottleneck shows up
// as bounded packet loss with bounded memory, not unbounded queueing
// masquerading as a multi-second "ramp-up".
//
// The mutex serializes ALL peers' submissions of one class device-wide
// (necessary since encQ.c is itself shared across peers, and only a
// single lock lets "check both channels have room" and "send to both"
// happen as one atomic step) -- a real scalability cost under many
// concurrent peers, but the critical section is just two length checks
// and two non-blocking sends, negligible next to the actual encryption
// work. Given melnode's mesh sizes so far (see PROJECT_STATE.md), this
// is judged an acceptable tradeoff against the alternative just
// described.
func (peer *Peer) submit(elemsContainer *QueueOutboundElementsContainer, isControl bool) {
	outboundQ := peer.queue.outbound
	encQ := peer.device.queue.encryption
	mu := &peer.device.queue.dataSubmitMu
	class := "data"
	if isControl {
		outboundQ = peer.queue.controlOutbound
		encQ = peer.device.queue.controlEncryption
		mu = &peer.device.queue.controlSubmitMu
		class = "control"
	}

	mu.Lock()
	if len(outboundQ.c) >= cap(outboundQ.c) || len(encQ.c) >= cap(encQ.c) {
		mu.Unlock()
		n := len(elemsContainer.elems)
		for _, elem := range elemsContainer.elems {
			peer.device.PutMessageBuffer(elem.buffer)
			peer.device.PutOutboundElement(elem)
		}
		peer.device.PutOutboundElementsContainer(elemsContainer)
		peer.device.log.Verbosef("%v - %s outbound/encryption queue full, dropping newest batch (%d packets)", peer, class, n)
		return
	}
	// Neither send below can actually block in steady-state operation:
	// capacity on both channels was just confirmed under this same
	// lock, and both channels are otherwise only ever written to under
	// this same per-class lock (device-wide for encQ.c, which is
	// shared; per-peer-but-still-under-this-lock for outboundQ.c). One
	// narrow exception: peer.Stop() sends a nil sentinel directly into
	// outboundQ.c, outside this lock, to unblock RoutineSequentialSender
	// -- a one-time event at final shutdown, not a recurring source of
	// contention, and not expected to coincide with this function still
	// being called for the same peer.
	outboundQ.c <- elemsContainer
	encQ.c <- elemsContainer
	mu.Unlock()
}

// SendStagedPackets drains the control queue fully before even looking
// at data -- see drainStaged and this file's top comments on
// StagePackets/submit for how that priority is actually enforced.
func (peer *Peer) SendStagedPackets() {
	if !peer.device.isUp() {
		return
	}
	peer.drainStaged(peer.queue.stagedControl, true)
	peer.drainStaged(peer.queue.staged, false)
}

// drainStaged is SendStagedPackets' original single-queue body,
// parameterized by which queue (and therefore which class) it's
// draining -- see submit() for where that class actually matters.
// Behavior per container is otherwise unchanged from before the
// control/data split: nonce assignment against the current keypair,
// with any tail that outruns RejectAfterMessages re-staged
// (peer.StagePackets) for the next keypair rather than dropped.
func (peer *Peer) drainStaged(ch chan *QueueOutboundElementsContainer, isControl bool) {
top:
	if len(ch) == 0 {
		return
	}

	keypair := peer.keypairs.Current()
	if keypair == nil || keypair.sendNonce.Load() >= RejectAfterMessages || time.Since(keypair.created) >= RejectAfterTime {
		peer.SendHandshakeInitiation(false)
		return
	}

	for {
		select {
		case elemsContainer := <-ch:
			i := 0
			var elemsContainerOOO *QueueOutboundElementsContainer
			for _, elem := range elemsContainer.elems {
				elem.peer = peer
				elem.nonce = keypair.sendNonce.Add(1) - 1
				if elem.nonce >= RejectAfterMessages {
					keypair.sendNonce.Store(RejectAfterMessages)
					if elemsContainerOOO == nil {
						elemsContainerOOO = peer.device.GetOutboundElementsContainer()
						elemsContainerOOO.isControl = isControl
					}
					elemsContainerOOO.elems = append(elemsContainerOOO.elems, elem)
					continue
				} else {
					elemsContainer.elems[i] = elem
					i++
				}

				elem.keypair = keypair
			}
			elemsContainer.Lock()
			elemsContainer.elems = elemsContainer.elems[:i]

			if elemsContainerOOO != nil {
				peer.StagePackets(elemsContainerOOO) // XXX: Out of order, but we can't front-load go chans
			}

			if len(elemsContainer.elems) == 0 {
				peer.device.PutOutboundElementsContainer(elemsContainer)
				goto top
			}

			// add to parallel and sequential queue
			if peer.isRunning.Load() {
				peer.submit(elemsContainer, isControl)
			} else {
				for _, elem := range elemsContainer.elems {
					peer.device.PutMessageBuffer(elem.buffer)
					peer.device.PutOutboundElement(elem)
				}
				peer.device.PutOutboundElementsContainer(elemsContainer)
			}

			if elemsContainerOOO != nil {
				goto top
			}
		default:
			return
		}
	}
}

func (peer *Peer) FlushStagedPackets() {
	drain := func(ch chan *QueueOutboundElementsContainer) {
		for {
			select {
			case elemsContainer := <-ch:
				for _, elem := range elemsContainer.elems {
					peer.device.PutMessageBuffer(elem.buffer)
					peer.device.PutOutboundElement(elem)
				}
				peer.device.PutOutboundElementsContainer(elemsContainer)
			default:
				return
			}
		}
	}
	drain(peer.queue.stagedControl)
	drain(peer.queue.staged)
}

func calculatePaddingSize(packetSize, mtu int) int {
	lastUnit := packetSize
	if mtu == 0 {
		return ((lastUnit + PaddingMultiple - 1) & ^(PaddingMultiple - 1)) - lastUnit
	}
	if lastUnit > mtu {
		lastUnit %= mtu
	}
	paddedSize := ((lastUnit + PaddingMultiple - 1) & ^(PaddingMultiple - 1))
	if paddedSize > mtu {
		paddedSize = mtu
	}
	return paddedSize - lastUnit
}

/* Encrypts the elements in the queue
 * and marks them for sequential consumption (by releasing the mutex)
 *
 * Obs. One instance per core
 */
func (device *Device) RoutineEncryption(id int) {
	var paddingZeros [PaddingMultiple]byte
	var nonce [chacha20poly1305.NonceSize]byte

	defer device.log.Verbosef("Routine: encryption worker %d - stopped", id)
	device.log.Verbosef("Routine: encryption worker %d - started", id)

	process := func(elemsContainer *QueueOutboundElementsContainer) {
		for _, elem := range elemsContainer.elems {
			// populate header fields
			header := elem.buffer[:MessageTransportHeaderSize]

			fieldType := header[0:4]
			fieldReceiver := header[4:8]
			fieldNonce := header[8:16]

			binary.LittleEndian.PutUint32(fieldType, MessageTransportType)
			binary.LittleEndian.PutUint32(fieldReceiver, elem.keypair.remoteIndex)
			binary.LittleEndian.PutUint64(fieldNonce, elem.nonce)

			// pad content to multiple of 16. Bounded by the device-wide
			// MTU (config: mtu, Device.mtu) rather than a per-tun MTU:
			// RoutineEncryption is shared across every peer-tun, so unlike
			// wireguard-go (one tun) there's no single "the" MTU to read
			// here -- see PROJECT_STATE.md's "Padding" section. elem.packet
			// still carries our own 4-byte routing header at this point, so
			// the bound is the tun MTU plus headerSize.
			paddingSize := calculatePaddingSize(len(elem.packet), device.mtu+headerSize)
			elem.packet = append(elem.packet, paddingZeros[:paddingSize]...)

			// encrypt content and release to consumer

			binary.LittleEndian.PutUint64(nonce[4:], elem.nonce)
			elem.packet = elem.keypair.send.Seal(
				header,
				nonce[:],
				elem.packet,
				nil,
			)
		}
		elemsContainer.Unlock()
	}

	// Control (liveness/path-vector) is drained first, and fully,
	// before data is even looked at -- same priority pattern as
	// Peer.drainStaged/SendStagedPackets (send.go), applied here to the
	// device-wide encryption step too, since it's shared across every
	// peer. See PROJECT_STATE.md's backpressure section.
	for {
		select {
		case elemsContainer, ok := <-device.queue.controlEncryption.c:
			if !ok {
				return
			}
			process(elemsContainer)
			continue
		default:
		}
		select {
		case elemsContainer, ok := <-device.queue.controlEncryption.c:
			if !ok {
				return
			}
			process(elemsContainer)
		case elemsContainer, ok := <-device.queue.encryption.c:
			if !ok {
				return
			}
			process(elemsContainer)
		}
	}
}

func (peer *Peer) RoutineSequentialSender(maxBatchSize int) {
	device := peer.device
	defer func() {
		defer device.log.Verbosef("%v - Routine: sequential sender - stopped", peer)
		peer.stopping.Done()
	}()
	device.log.Verbosef("%v - Routine: sequential sender - started", peer)

	bufs := make([][]byte, 0, maxBatchSize)

	// process handles one already-encrypted container: wait for the
	// encryption worker's Unlock (this is what keeps packets in send
	// order despite parallel encryption -- see RoutineEncryption),
	// batch-send it, and return its resources to the pool. Returns true
	// if the caller should stop (the nil sentinel from Peer.Stop()).
	process := func(elemsContainer *QueueOutboundElementsContainer) (stop bool) {
		bufs = bufs[:0]
		if elemsContainer == nil {
			return true
		}
		if !peer.isRunning.Load() {
			// peer has been stopped; return re-usable elems to the shared pool.
			// This is an optimization only. It is possible for the peer to be stopped
			// immediately after this check, in which case, elem will get processed.
			// The timers and SendBuffers code are resilient to a few stragglers.
			// TODO: rework peer shutdown order to ensure
			// that we never accidentally keep timers alive longer than necessary.
			elemsContainer.Lock()
			for _, elem := range elemsContainer.elems {
				device.PutMessageBuffer(elem.buffer)
				device.PutOutboundElement(elem)
			}
			device.PutOutboundElementsContainer(elemsContainer)
			return false
		}
		dataSent := false
		elemsContainer.Lock()
		for _, elem := range elemsContainer.elems {
			if len(elem.packet) != MessageKeepaliveSize {
				dataSent = true
			}
			bufs = append(bufs, elem.packet)
		}

		peer.timersAnyAuthenticatedPacketTraversal()
		peer.timersAnyAuthenticatedPacketSent()

		err := peer.SendBuffers(bufs)
		if dataSent {
			peer.timersDataSent()
		}
		for _, elem := range elemsContainer.elems {
			device.PutMessageBuffer(elem.buffer)
			device.PutOutboundElement(elem)
		}
		device.PutOutboundElementsContainer(elemsContainer)
		if err != nil {
			var errGSO conn.ErrUDPGSODisabled
			if errors.As(err, &errGSO) {
				device.log.Verbosef(err.Error())
				err = errGSO.RetryErr
			}
		}
		if err != nil {
			device.log.Errorf("%v - Failed to send data packets: %v", peer, err)
			return false
		}

		peer.keepKeyFreshSending()
		return false
	}

	// Same priority pattern as everywhere else in the control/data
	// split (send.go's StagePackets/drainStaged, RoutineEncryption
	// above): controlOutbound is checked -- and, whenever it has
	// anything, fully drained -- before outbound (data) is even looked
	// at. See PROJECT_STATE.md's backpressure section.
	for {
		select {
		case elemsContainer := <-peer.queue.controlOutbound.c:
			if process(elemsContainer) {
				return
			}
			continue
		default:
		}
		select {
		case elemsContainer := <-peer.queue.controlOutbound.c:
			if process(elemsContainer) {
				return
			}
		case elemsContainer := <-peer.queue.outbound.c:
			if process(elemsContainer) {
				return
			}
		}
	}
}
