/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package main

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.zx2c4.com/wireguard/conn"

	"melnode/dpproto"
)

// Multi-hop forwards are grouped by next-hop *Peer and staged as one
// batch per next hop per round, not handled one packet at a time -- see
// the steal site below for why: one StagePackets+SendStagedPackets call
// per *packet* was serializing what should be one batched send per
// round, which profiling (on real hardware, not the sandbox) showed as
// a large share of RoutineSequentialSender's time going into raw
// write(2) syscalls one packet at a time instead of a batched sendmmsg,
// the way freshly-originated traffic already gets via
// RoutineReadFromTUN's own single elemsForPeer container per round.

type QueueHandshakeElement struct {
	msgType  uint32
	packet   []byte
	endpoint conn.Endpoint
	buffer   *[MaxMessageSize]byte
}

type QueueInboundElement struct {
	buffer   *[MaxMessageSize]byte
	packet   []byte
	counter  uint64
	keypair  *Keypair
	endpoint conn.Endpoint
}

type QueueInboundElementsContainer struct {
	sync.Mutex
	elems []*QueueInboundElement
}

// clearPointers clears elem fields that contain pointers.
// This makes the garbage collector's life easier and
// avoids accidentally keeping other objects around unnecessarily.
// It also reduces the possible collateral damage from use-after-free bugs.
func (elem *QueueInboundElement) clearPointers() {
	elem.buffer = nil
	elem.packet = nil
	elem.keypair = nil
	elem.endpoint = nil
}

/* Called when a new authenticated message has been received
 *
 * NOTE: Not thread safe, but called by sequential receiver!
 */
func (peer *Peer) keepKeyFreshReceiving() {
	if peer.timers.sentLastMinuteHandshake.Load() {
		return
	}
	keypair := peer.keypairs.Current()
	if keypair != nil && keypair.isInitiator && time.Since(keypair.created) > (RejectAfterTime-KeepaliveTimeout-RekeyTimeout) {
		peer.timers.sentLastMinuteHandshake.Store(true)
		peer.SendHandshakeInitiation(false)
	}
}

/* Receives incoming datagrams for the device
 *
 * Every time the bind is updated a new routine is started for
 * IPv4 and IPv6 (separately)
 */
// submitInbound is the receive-side mirror of Peer.submit (send.go):
// an atomic check-both-then-send into peer.queue.inbound (per-peer,
// feeds RoutineSequentialReceiver) and device.queue.decryption (shared
// device-wide, feeds the decryption workers), guarded by
// device.queue.decryptionSubmitMu. Either both channels have room and
// both sends happen, or neither does and the batch is tail-dropped.
//
// This used to be two plain blocking channel sends, same anti-pattern
// Peer.submit had on the send side (see its own doc comment) --
// found the same way, one step later: fixing the send side alone
// didn't resolve the originally reported symptom (heavy loss for a
// few seconds at the start of a multi-Gbit/s UDP test) because that
// specific repro had the *receiving* node as the actual bottleneck.
// When decryption genuinely can't keep pace, even briefly, a blocking
// send into device.queue.decryption.c stalls the caller -- which here
// is RoutineReceiveIncoming, the goroutine actually reading off the
// UDP socket. Stalling that doesn't queue politely; it means the
// socket stops being drained, and the OS's own UDP receive buffer
// overflows and drops datagrams before melnode ever sees them, which
// looks exactly like the reported symptom and isn't visible in any of
// melnode's own queue-full counters. See PROJECT_STATE.md.
func (device *Device) submitInbound(peer *Peer, elemsContainer *QueueInboundElementsContainer) {
	device.queue.decryptionSubmitMu.Lock()
	if len(peer.queue.inbound.c) >= cap(peer.queue.inbound.c) || len(device.queue.decryption.c) >= cap(device.queue.decryption.c) {
		device.queue.decryptionSubmitMu.Unlock()
		n := len(elemsContainer.elems)
		for _, elem := range elemsContainer.elems {
			device.PutMessageBuffer(elem.buffer)
			device.PutInboundElement(elem)
		}
		device.PutInboundElementsContainer(elemsContainer)
		device.stats.rxQueueFull.Add(uint64(n))
		return
	}
	// Neither send below can actually block: capacity on both channels
	// was just confirmed under this same lock, and both are otherwise
	// only ever written to under it -- with the same one narrow
	// exception Peer.submit's doc comment notes (peer.Stop() sends a
	// nil sentinel into peer.queue.inbound.c directly, at final
	// shutdown only).
	peer.queue.inbound.c <- elemsContainer
	device.queue.decryption.c <- elemsContainer
	device.queue.decryptionSubmitMu.Unlock()
}

func (device *Device) RoutineReceiveIncoming(maxBatchSize int, recv conn.ReceiveFunc) {
	defer func() {
		device.queue.decryption.wg.Done()
		device.queue.handshake.wg.Done()
		device.net.stopping.Done()
	}()

	// receive datagrams until conn is closed

	var (
		bufsArrs    = make([]*[MaxMessageSize]byte, maxBatchSize)
		bufs        = make([][]byte, maxBatchSize)
		err         error
		sizes       = make([]int, maxBatchSize)
		count       int
		endpoints   = make([]conn.Endpoint, maxBatchSize)
		deathSpiral int
		elemsByPeer = make(map[*Peer]*QueueInboundElementsContainer, maxBatchSize)
	)

	for i := range bufsArrs {
		bufsArrs[i] = device.GetMessageBuffer()
		bufs[i] = bufsArrs[i][:]
	}

	defer func() {
		for i := 0; i < maxBatchSize; i++ {
			if bufsArrs[i] != nil {
				device.PutMessageBuffer(bufsArrs[i])
			}
		}
	}()

	for {
		count, err = recv(bufs, sizes, endpoints)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if neterr, ok := err.(net.Error); ok && !neterr.Temporary() {
				return
			}
			if deathSpiral < 10 {
				deathSpiral++
				time.Sleep(time.Second / 3)
				continue
			}
			return
		}
		deathSpiral = 0

		// handle each packet in the batch
		for i, size := range sizes[:count] {
			if size < MinMessageSize {
				continue
			}

			// check size of packet

			packet := bufsArrs[i][:size]
			msgType := binary.LittleEndian.Uint32(packet[:4])

			switch msgType {

			// check if transport

			case MessageTransportType:

				// check size

				if len(packet) < MessageTransportSize {
					continue
				}

				// lookup key pair

				receiver := binary.LittleEndian.Uint32(
					packet[MessageTransportOffsetReceiver:MessageTransportOffsetCounter],
				)
				value := device.indexTable.Lookup(receiver)
				keypair := value.keypair
				if keypair == nil {
					continue
				}

				// check keypair expiry

				if keypair.created.Add(RejectAfterTime).Before(time.Now()) {
					continue
				}

				// create work element
				peer := value.peer
				elem := device.GetInboundElement()
				elem.packet = packet
				elem.buffer = bufsArrs[i]
				elem.keypair = keypair
				elem.endpoint = endpoints[i]
				elem.counter = 0

				elemsForPeer, ok := elemsByPeer[peer]
				if !ok {
					elemsForPeer = device.GetInboundElementsContainer()
					elemsForPeer.Lock()
					elemsByPeer[peer] = elemsForPeer
				}
				elemsForPeer.elems = append(elemsForPeer.elems, elem)
				bufsArrs[i] = device.GetMessageBuffer()
				bufs[i] = bufsArrs[i][:]
				continue

			// otherwise it is a fixed size & handshake related packet

			case MessageInitiationType:
				if len(packet) != MessageInitiationSize {
					continue
				}

			case MessageResponseType:
				if len(packet) != MessageResponseSize {
					continue
				}

			case MessageCookieReplyType:
				if len(packet) != MessageCookieReplySize {
					continue
				}

			default:
				continue
			}

			select {
			case device.queue.handshake.c <- QueueHandshakeElement{
				msgType:  msgType,
				buffer:   bufsArrs[i],
				packet:   packet,
				endpoint: endpoints[i],
			}:
				bufsArrs[i] = device.GetMessageBuffer()
				bufs[i] = bufsArrs[i][:]
			default:
			}
		}
		for peer, elemsContainer := range elemsByPeer {
			if peer.isRunning.Load() {
				device.submitInbound(peer, elemsContainer)
			} else {
				for _, elem := range elemsContainer.elems {
					device.PutMessageBuffer(elem.buffer)
					device.PutInboundElement(elem)
				}
				device.PutInboundElementsContainer(elemsContainer)
			}
			delete(elemsByPeer, peer)
		}
	}
}

func (device *Device) RoutineDecryption(id int) {
	var nonce [chacha20poly1305.NonceSize]byte

	for elemsContainer := range device.queue.decryption.c {
		for _, elem := range elemsContainer.elems {
			// split message into fields
			counter := elem.packet[MessageTransportOffsetCounter:MessageTransportOffsetContent]
			content := elem.packet[MessageTransportOffsetContent:]

			// decrypt and release to consumer
			var err error
			elem.counter = binary.LittleEndian.Uint64(counter)
			// copy counter to nonce
			binary.LittleEndian.PutUint64(nonce[0x4:0xc], elem.counter)
			elem.packet, err = elem.keypair.receive.Open(
				content[:0],
				nonce[:],
				content,
				nil,
			)
			if err != nil {
				elem.packet = nil
			}
		}
		elemsContainer.Unlock()
	}
}

/* Handles incoming packets related to handshake
 */
func (device *Device) RoutineHandshake(id int) {
	defer func() {
		device.queue.encryption.wg.Done()
	}()

	for elem := range device.queue.handshake.c {

		// handle cookie fields and ratelimiting

		switch elem.msgType {

		case MessageCookieReplyType:

			// unmarshal packet

			var reply MessageCookieReply
			err := reply.unmarshal(elem.packet)
			if err != nil {
				goto skip
			}

			// lookup peer from index

			entry := device.indexTable.Lookup(reply.Receiver)

			if entry.peer == nil {
				goto skip
			}

			// consume reply

			if peer := entry.peer; peer.isRunning.Load() {
				if !peer.cookieGenerator.ConsumeReply(&reply) {
				}
			}

			goto skip

		case MessageInitiationType, MessageResponseType:

			// check mac fields and maybe ratelimit

			if !device.cookieChecker.CheckMAC1(elem.packet) {
				goto skip
			}

			// endpoints destination address is the source of the datagram

			if device.IsUnderLoad() {

				// verify MAC2 field

				if !device.cookieChecker.CheckMAC2(elem.packet, elem.endpoint.DstToBytes()) {
					device.SendHandshakeCookie(&elem)
					goto skip
				}

				// check ratelimiter

				if !device.rate.limiter.Allow(elem.endpoint.DstIP()) {
					goto skip
				}
			}

		default:
			device.log.Errorf("Invalid packet ended up in the handshake queue")
			goto skip
		}

		// handle handshake initiation/response content

		switch elem.msgType {
		case MessageInitiationType:

			// unmarshal

			var msg MessageInitiation
			err := msg.unmarshal(elem.packet)
			if err != nil {
				device.log.Errorf("Failed to decode initiation message")
				goto skip
			}

			// consume initiation

			peer := device.ConsumeMessageInitiation(&msg)
			if peer == nil {
				goto skip
			}

			// update timers

			peer.timersAnyAuthenticatedPacketTraversal()
			peer.timersAnyAuthenticatedPacketReceived()

			// update endpoint
			peer.SetEndpointFromPacket(elem.endpoint)

			peer.rxBytes.Add(uint64(len(elem.packet)))

			peer.SendHandshakeResponse()

		case MessageResponseType:

			// unmarshal

			var msg MessageResponse
			err := msg.unmarshal(elem.packet)
			if err != nil {
				device.log.Errorf("Failed to decode response message")
				goto skip
			}

			// consume response

			peer := device.ConsumeMessageResponse(&msg)
			if peer == nil {
				goto skip
			}

			// update endpoint
			peer.SetEndpointFromPacket(elem.endpoint)

			peer.rxBytes.Add(uint64(len(elem.packet)))

			// update timers

			peer.timersAnyAuthenticatedPacketTraversal()
			peer.timersAnyAuthenticatedPacketReceived()

			// derive keypair

			err = peer.BeginSymmetricSession()

			if err != nil {
				device.log.Errorf("%v - Failed to derive keypair: %v", peer, err)
				goto skip
			}

			peer.timersSessionDerived()
			peer.timersHandshakeComplete()
			peer.SendKeepalive()
		}
	skip:
		device.PutMessageBuffer(elem.buffer)
	}
}

func (peer *Peer) RoutineSequentialReceiver(maxBatchSize int) {
	device := peer.device
	defer func() {
		peer.stopping.Done()
	}()

	localByPeer := make(map[*tunWriter][]tunWriteItem)
	forwardsByNextHop := make(map[*Peer]*QueueOutboundElementsContainer)

	for elemsContainer := range peer.queue.inbound.c {
		if elemsContainer == nil {
			return
		}
		elemsContainer.Lock()
		validTailPacket := -1
		dataPacketReceived := false
		rxBytesLen := uint64(0)
		for i, elem := range elemsContainer.elems {
			if elem.packet == nil {
				// decryption failed
				continue
			}

			if !elem.keypair.replayFilter.ValidateCounter(elem.counter, RejectAfterMessages) {
				continue
			}

			validTailPacket = i
			if peer.ReceivedWithKeypair(elem.keypair) {
				peer.SetEndpointFromPacket(elem.endpoint)
				peer.timersHandshakeComplete()
				peer.SendStagedPackets()
			}
			rxBytesLen += uint64(len(elem.packet) + MinMessageSize)

			if len(elem.packet) == 0 {
				continue
			}
			dataPacketReceived = true

			// melnode's own header + routing, replacing wireguard-go's
			// allowedips.Lookup + IP-padding-trim here (see
			// PROJECT_STATE.md's "What replaces the two IP-specific
			// blocks" and its "Padding" section). No anti-spoof check:
			// melnode's security model is "whichever peer's keypair
			// decrypted this is who sent it", full stop -- there's no
			// allowed-ips-style "this peer may only claim these
			// addresses" concept, deliberately not invented here.
			if len(elem.packet) < headerSize {
				device.stats.rxBad.Add(1)
				continue
			}
			proto, src, dst := elem.packet[0], elem.packet[1], elem.packet[2]
			if proto != 0 {
				// Any non-zero proto is a control packet (link
				// liveness, path-vector routing, whatever comes
				// next). The data plane deliberately knows nothing
				// about their formats: it hands the payload up to the
				// control plane tagged with the link it arrived on,
				// and moves on. Never forwarded, whatever dst says --
				// the control plane decides what (if anything) to do.
				device.ctl.Punt(dpproto.Punt{
					Ingress: peer.id,
					Proto:   proto,
					Src:     src,
					Dst:     dst,
					TTL:     elem.packet[hdrOffTTL],
					Payload: elem.packet[headerSize:],
				})
				continue
			}

			// proto 0, a tunneled IP packet: trim, then deliver locally
			// or forward, decided by deliverOrForward (routing.go).
			result, pkt, nextHop := device.deliverOrForward(elem.packet)
			switch result {
			case fwdLocal:
				w, ok := device.router.LookupTunWriter(uint32(src))
				if !ok {
					device.stats.rxNoTun.Add(1)
					continue
				}
				elem.packet = pkt
				localByPeer[w] = append(localByPeer[w], tunWriteItem{buffer: elem.buffer, packet: elem.packet})
				// nil it out here, same as the forwarding branch below --
				// ownership of the buffer now belongs to the tun writer
				// (or, if its queue is full, to the drop path in this
				// function's trailing flush), not to this function's own
				// per-elem cleanup loop at the end of the round.
				elem.buffer = nil
			case fwdForward:
				// Multi-hop forward: hand the whole still-proto=0-headed
				// packet back into the outbound path for re-encryption
				// onto the next link, stealing elem.buffer outright
				// rather than copying (inbound/outbound elements share
				// one buffer pool, so this is a pointer handoff). Batch
				// into one QueueOutboundElementsContainer per next-hop
				// peer per round (forwardsByNextHop) instead of staging
				// each packet individually -- see the comment where that
				// map is declared for why that matters.
				elem.packet = pkt
				outElem := device.GetOutboundElement() // NewOutboundElement would also fetch a *fresh* buffer, which we don't want -- we already have one
				outElem.buffer = elem.buffer
				outElem.packet = elem.packet
				fc, ok := forwardsByNextHop[nextHop]
				if !ok {
					fc = device.GetOutboundElementsContainer()
					fc.forwarded = true
					forwardsByNextHop[nextHop] = fc
				}
				fc.elems = append(fc.elems, outElem)
				// nil it out here so the cleanup loop below (which
				// still runs for every elem in this container,
				// forwarded or not) knows not to return it to the pool
				// out from under the outbound element that now owns it.
				elem.buffer = nil
			}
		}

		peer.rxBytes.Add(rxBytesLen)
		if validTailPacket >= 0 {
			peer.SetEndpointFromPacket(elemsContainer.elems[validTailPacket].endpoint)
			peer.keepKeyFreshReceiving()
			peer.timersAnyAuthenticatedPacketTraversal()
			peer.timersAnyAuthenticatedPacketReceived()
		}
		if dataPacketReceived {
			peer.timersDataReceived()
		}
		for w, items := range localByPeer {
			select {
			case w.ch <- items:
			default:
				// Standard router behavior under congestion: tail-drop
				// (see PROJECT_STATE.md's backpressure section) --
				// whatever's on the other end of this tun isn't
				// draining fast enough, so the newest round is what
				// gets dropped rather than blocking this receiver (and
				// delaying its *next* round, including any
				// liveness/path-vector packets in it) waiting for
				// room.
				device.stats.rxTunFull.Add(uint64(len(items)))
				for _, it := range items {
					device.PutMessageBuffer(it.buffer)
				}
			}
			delete(localByPeer, w)
		}
		for peer, fc := range forwardsByNextHop {
			peer.StagePackets(fc)
			peer.SendStagedPackets()
			delete(forwardsByNextHop, peer)
		}
		for _, elem := range elemsContainer.elems {
			if elem.buffer != nil { // nil if this elem's buffer was stolen for a multi-hop forward above
				device.PutMessageBuffer(elem.buffer)
			}
			device.PutInboundElement(elem)
		}
		device.PutInboundElementsContainer(elemsContainer)
	}
}
