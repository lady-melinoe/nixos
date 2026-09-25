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

func (elem *QueueInboundElement) clearPointers() {
	elem.buffer = nil
	elem.packet = nil
	elem.keypair = nil
	elem.endpoint = nil
}

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

		for i, size := range sizes[:count] {
			if size < MinMessageSize {
				continue
			}

			packet := bufsArrs[i][:size]
			msgType := binary.LittleEndian.Uint32(packet[:4])

			switch msgType {
			case MessageTransportType:

				if len(packet) < MessageTransportSize {
					continue
				}

				receiver := binary.LittleEndian.Uint32(
					packet[MessageTransportOffsetReceiver:MessageTransportOffsetCounter],
				)
				value := device.indexTable.Lookup(receiver)
				keypair := value.keypair
				if keypair == nil {
					continue
				}

				if keypair.created.Add(RejectAfterTime).Before(time.Now()) {
					continue
				}

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
			counter := elem.packet[MessageTransportOffsetCounter:MessageTransportOffsetContent]
			content := elem.packet[MessageTransportOffsetContent:]

			var err error
			elem.counter = binary.LittleEndian.Uint64(counter)
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

func (device *Device) RoutineHandshake(id int) {
	defer func() {
		device.queue.encryption.wg.Done()
		device.queue.controlEncryption.wg.Done()
	}()

	for elem := range device.queue.handshake.c {
		switch elem.msgType {
		case MessageCookieReplyType:

			var reply MessageCookieReply
			err := reply.unmarshal(elem.packet)
			if err != nil {
				goto skip
			}

			entry := device.indexTable.Lookup(reply.Receiver)

			if entry.peer == nil {
				goto skip
			}

			if peer := entry.peer; peer.isRunning.Load() {
				if !peer.cookieGenerator.ConsumeReply(&reply) {
				}
			}

			goto skip

		case MessageInitiationType, MessageResponseType:

			if !device.cookieChecker.CheckMAC1(elem.packet) {
				goto skip
			}

			if device.IsUnderLoad() {
				if !device.cookieChecker.CheckMAC2(elem.packet, elem.endpoint.DstToBytes()) {
					device.SendHandshakeCookie(&elem)
					goto skip
				}

				if !device.rate.limiter.Allow(elem.endpoint.DstIP()) {
					goto skip
				}
			}

		default:
			device.log.Errorf("Invalid packet ended up in the handshake queue")
			goto skip
		}

		switch elem.msgType {
		case MessageInitiationType:

			var msg MessageInitiation
			err := msg.unmarshal(elem.packet)
			if err != nil {
				device.log.Errorf("Failed to decode initiation message")
				goto skip
			}

			peer := device.ConsumeMessageInitiation(&msg)
			if peer == nil {
				goto skip
			}

			peer.timersAnyAuthenticatedPacketTraversal()
			peer.timersAnyAuthenticatedPacketReceived()

			peer.SetEndpointFromPacket(elem.endpoint)

			peer.rxBytes.Add(uint64(len(elem.packet)))

			peer.SendHandshakeResponse()

		case MessageResponseType:

			var msg MessageResponse
			err := msg.unmarshal(elem.packet)
			if err != nil {
				device.log.Errorf("Failed to decode response message")
				goto skip
			}

			peer := device.ConsumeMessageResponse(&msg)
			if peer == nil {
				goto skip
			}

			peer.SetEndpointFromPacket(elem.endpoint)

			peer.rxBytes.Add(uint64(len(elem.packet)))

			peer.timersAnyAuthenticatedPacketTraversal()
			peer.timersAnyAuthenticatedPacketReceived()

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

			if len(elem.packet) < headerSize {
				device.stats.rxBad.Add(1)
				continue
			}
			proto, src, dst := elem.packet[0], elem.packet[1], elem.packet[2]
			if proto != 0 {
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
				elem.buffer = nil
			case fwdForward:
				elem.packet = pkt
				outElem := device.GetOutboundElement()
				outElem.buffer = elem.buffer
				outElem.packet = elem.packet
				fc, ok := forwardsByNextHop[nextHop]
				if !ok {
					fc = device.GetOutboundElementsContainer()
					fc.forwarded = true
					forwardsByNextHop[nextHop] = fc
				}
				fc.elems = append(fc.elems, outElem)
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
			if elem.buffer != nil {
				device.PutMessageBuffer(elem.buffer)
			}
			device.PutInboundElement(elem)
		}
		device.PutInboundElementsContainer(elemsContainer)
	}
}
