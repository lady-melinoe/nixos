package main

import "encoding/binary"

func trimIP(pkt []byte) ([]byte, bool) {
	ip := pkt[headerSize:]
	switch {
	case len(ip) >= 20 && ip[0]>>4 == 4:
		length := int(binary.BigEndian.Uint16(ip[IPv4offsetTotalLength : IPv4offsetTotalLength+2]))
		if length > len(ip) || length < 20 {
			return nil, false
		}
		return pkt[:headerSize+length], true
	case len(ip) >= 40 && ip[0]>>4 == 6:
		length := int(binary.BigEndian.Uint16(ip[IPv6offsetPayloadLength:IPv6offsetPayloadLength+2])) + 40
		if length > len(ip) {
			return nil, false
		}
		return pkt[:headerSize+length], true
	}
	return nil, false
}

func (device *Device) routeAndSend(dst uint32, elems *QueueOutboundElementsContainer) bool {
	peer := device.router.resolveNextHop(dst)
	if peer == nil || !peer.isRunning.Load() {
		return false
	}
	peer.StagePackets(elems)
	peer.SendStagedPackets()
	return true
}

type forwardResult int

const (
	fwdDrop forwardResult = iota
	fwdLocal
	fwdForward
)

func (device *Device) deliverOrForward(pkt []byte) (forwardResult, []byte, *Peer) {
	pkt, ok := trimIP(pkt)
	if !ok {
		device.stats.rxBad.Add(1)
		return fwdDrop, nil, nil
	}
	if uint32(pkt[2]) == device.localID {
		return fwdLocal, pkt, nil
	}
	if pkt[hdrOffTTL] <= 1 {
		device.stats.rxTTL.Add(1)
		return fwdDrop, nil, nil
	}
	nextHop := device.router.resolveNextHop(uint32(pkt[2]))
	if nextHop == nil {
		device.stats.rxNoRoute.Add(1)
		return fwdDrop, nil, nil
	}
	pkt[hdrOffTTL]--
	return fwdForward, pkt, nextHop
}
