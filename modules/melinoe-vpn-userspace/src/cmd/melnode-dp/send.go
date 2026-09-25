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

type QueueOutboundElement struct {
	buffer  *[MaxMessageSize]byte
	packet  []byte
	nonce   uint64
	keypair *Keypair
	peer    *Peer
}

type QueueOutboundElementsContainer struct {
	sync.Mutex
	elems     []*QueueOutboundElement
	isControl bool
	forwarded bool
}

func (device *Device) NewOutboundElement() *QueueOutboundElement {
	elem := device.GetOutboundElement()
	elem.buffer = device.GetMessageBuffer()
	elem.nonce = 0
	return elem
}

func (elem *QueueOutboundElement) clearPointers() {
	elem.buffer = nil
	elem.packet = nil
	elem.keypair = nil
	elem.peer = nil
}

func (peer *Peer) SendKeepalive() {
	if len(peer.queue.staged) == 0 && peer.isRunning.Load() {
		elem := peer.device.NewOutboundElement()
		elemsContainer := peer.device.GetOutboundElementsContainer()
		elemsContainer.elems = append(elemsContainer.elems, elem)
		select {
		case peer.queue.staged <- elemsContainer:
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
	if !peer.isRunning.Load() {
		return nil
	}

	if !peer.hasEndpoint() {
		return nil
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

	err = peer.SendBuffers([][]byte{packet})
	if err != nil {
		peer.device.log.Errorf("%v - Failed to send handshake response: %v", peer, err)
	}
	return err
}

func (device *Device) SendHandshakeCookie(initiatingElem *QueueHandshakeElement) error {
	sender := binary.LittleEndian.Uint32(initiatingElem.packet[4:8])
	reply, err := device.cookieChecker.CreateReply(initiatingElem.packet, sender, initiatingElem.endpoint.DstToBytes())
	if err != nil {
		device.log.Errorf("Failed to create cookie reply: %v", err)
		return err
	}

	packet := make([]byte, MessageCookieReplySize)
	_ = reply.marshal(packet)
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

func (device *Device) RoutineReadFromTUN(peerID uint32, devTun tun.Device) {
	defer func() {
		device.queue.encryption.wg.Done()
	}()

	var (
		batchSize = devTun.BatchSize()
		readErr   error
		elems     = make([]*QueueOutboundElement, batchSize)
		bufs      = make([][]byte, batchSize)
		count     = 0
		sizes     = make([]int, batchSize)
		offset    = MessageTransportHeaderSize + headerSize
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
		count, readErr = devTun.Read(bufs, sizes, offset)
		if count > 0 {
			elemsForPeer := device.GetOutboundElementsContainer()
			for i := 0; i < count; i++ {
				if sizes[i] < 1 {
					continue
				}

				elem := elems[i]
				buf := elem.buffer[:]

				buf[offset-headerSize+0] = 0
				buf[offset-headerSize+1] = byte(device.localID)
				buf[offset-headerSize+2] = byte(peerID)
				buf[offset-headerSize+hdrOffTTL] = defaultTTL
				elem.packet = buf[offset-headerSize : offset+sizes[i]]

				elemsForPeer.elems = append(elemsForPeer.elems, elem)
				elems[i] = device.NewOutboundElement()
				bufs[i] = elems[i].buffer[:]
			}

			if len(elemsForPeer.elems) == 0 {
				device.PutOutboundElementsContainer(elemsForPeer)
			} else if !device.routeAndSend(peerID, elemsForPeer) {
				device.stats.txNoRoute.Add(uint64(len(elemsForPeer.elems)))
				device.freeOutbound(elemsForPeer)
			}
		}

		if readErr != nil {
			if errors.Is(readErr, tun.ErrTooManySegments) {
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

func (peer *Peer) StagePackets(elems *QueueOutboundElementsContainer) {
	ch := peer.queue.staged
	if elems.isControl {
		ch = peer.queue.stagedControl
	}

	select {
	case ch <- elems:
		return
	default:
	}

	peer.device.dropQueueFull(elems)
}

func (device *Device) dropQueueFull(elems *QueueOutboundElementsContainer) {
	n := uint64(len(elems.elems))
	if elems.forwarded {
		device.stats.rxQueueFull.Add(n)
	} else {
		device.stats.txQueueFull.Add(n)
	}
	device.freeOutbound(elems)
}

func (device *Device) freeOutbound(elems *QueueOutboundElementsContainer) {
	for _, elem := range elems.elems {
		device.PutMessageBuffer(elem.buffer)
		device.PutOutboundElement(elem)
	}
	device.PutOutboundElementsContainer(elems)
}

func (peer *Peer) submit(elemsContainer *QueueOutboundElementsContainer, isControl bool) {
	outboundQ := peer.queue.outbound
	encQ := peer.device.queue.encryption
	mu := &peer.device.queue.dataSubmitMu
	if isControl {
		outboundQ = peer.queue.controlOutbound
		encQ = peer.device.queue.controlEncryption
		mu = &peer.device.queue.controlSubmitMu
	}

	n := int64(len(elemsContainer.elems))
	mu.Lock()
	if len(outboundQ.c) >= cap(outboundQ.c) || len(encQ.c) >= cap(encQ.c) ||
		(!isControl && peer.dataInFlight.Load()+n > maxDataInFlight) {
		mu.Unlock()
		peer.device.dropQueueFull(elemsContainer)
		return
	}
	if !isControl {
		peer.dataInFlight.Add(n)
	}
	outboundQ.c <- elemsContainer
	encQ.c <- elemsContainer
	mu.Unlock()
}

func (peer *Peer) SendStagedPackets() {
	if !peer.device.isUp() {
		return
	}
	peer.drainStaged(peer.queue.stagedControl, true)
	peer.drainStaged(peer.queue.staged, false)
}

func (peer *Peer) drainStaged(ch chan *QueueOutboundElementsContainer, isControl bool) {
top:
	if len(ch) == 0 {
		return
	}

	keypair := peer.sendKeypair()
	if keypair == nil {
		peer.dropNoSession(ch, isControl)
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
				peer.StagePackets(elemsContainerOOO)
			}

			if len(elemsContainer.elems) == 0 {
				peer.device.PutOutboundElementsContainer(elemsContainer)
				goto top
			}

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

func (peer *Peer) hasEndpoint() bool {
	peer.endpoint.Lock()
	defer peer.endpoint.Unlock()
	return peer.endpoint.val != nil
}

func (peer *Peer) sendKeypair() *Keypair {
	keypair := peer.keypairs.Current()
	if keypair == nil || !peer.hasEndpoint() ||
		keypair.sendNonce.Load() >= RejectAfterMessages ||
		time.Since(keypair.created) >= RejectAfterTime {
		return nil
	}
	return keypair
}

func (peer *Peer) dropNoSession(ch chan *QueueOutboundElementsContainer, isControl bool) {
	initiate := false
	for {
		select {
		case elemsContainer := <-ch:
			for _, elem := range elemsContainer.elems {
				if len(elem.packet) == 0 {
					continue
				}
				initiate = true
				if isControl {
					peer.device.stats.injectSent.Add(^uint64(0))
					peer.device.stats.injectDropped.Add(1)
				} else {
					peer.device.stats.txNoRoute.Add(1)
				}
			}
			peer.device.freeOutbound(elemsContainer)
		default:
			if initiate {
				peer.SendHandshakeInitiation(false)
			}
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

func (device *Device) RoutineEncryption(id int) {
	var paddingZeros [PaddingMultiple]byte
	var nonce [chacha20poly1305.NonceSize]byte

	process := func(elemsContainer *QueueOutboundElementsContainer) {
		for _, elem := range elemsContainer.elems {
			header := elem.buffer[:MessageTransportHeaderSize]

			fieldType := header[0:4]
			fieldReceiver := header[4:8]
			fieldNonce := header[8:16]

			binary.LittleEndian.PutUint32(fieldType, MessageTransportType)
			binary.LittleEndian.PutUint32(fieldReceiver, elem.keypair.remoteIndex)
			binary.LittleEndian.PutUint64(fieldNonce, elem.nonce)

			paddingSize := calculatePaddingSize(len(elem.packet), device.mtu+headerSize)
			elem.packet = append(elem.packet, paddingZeros[:paddingSize]...)

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
		peer.stopping.Done()
	}()

	bufs := make([][]byte, 0, maxBatchSize)

	process := func(elemsContainer *QueueOutboundElementsContainer) (stop bool) {
		bufs = bufs[:0]
		if elemsContainer == nil {
			return true
		}
		if !elemsContainer.isControl {
			defer peer.dataInFlight.Add(-int64(len(elemsContainer.elems)))
		}
		if !peer.isRunning.Load() {
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
