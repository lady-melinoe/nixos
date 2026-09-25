package main

import "encoding/binary"

// routing.go holds the forwarding decisions, kept in the same shape as the
// kernel module's melnode_routing.c so a change to one is easy to carry to
// the other. Correspondence:
//
//	trimIP                  trim_ip
//	routeAndSend            melnode_routing_route_and_send (tun ingress half)
//	deliverOrForward        melnode_routing_deliver_or_forward
//	Peer.sendKeypair        the -ENOENT test in melnode_routing_send_now
//	Peer.dropNoSession      outq_work_fn's -ENOENT branch (send.go)
//	ctlHandler.Inject       melnode_nl_inject (ctl.go)
//
// What is deliberately different from the kernel: no DSCP/ECN/TOS handling,
// and packets are moved in batches (one container per next hop per round,
// parallel encryption, sendmmsg) instead of one item at a time through a
// per-link ring. The decisions, their order and the stat each drop is
// counted under are meant to be identical.

// trimIP cuts pkt (routing header + tunneled IP packet + padding) down to the
// IP packet's own declared length, dropping the transport padding. It reports
// false if pkt is not a well-formed IPv4/IPv6 packet.
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

// routeAndSend queues a container of freshly stamped tun-originated packets
// for dst's next hop. It returns false, having queued nothing, if dst has no
// route or its link isn't running; the caller counts that as tx_no_route and
// frees the container. A full queue is counted (tx_queue_full) and freed
// inside StagePackets.
func (device *Device) routeAndSend(dst uint32, elems *QueueOutboundElementsContainer) bool {
	// Resolved on every call, not cached per tun: a path-vector reroute of
	// dst to a different next hop takes effect on the very next round.
	peer := device.router.resolveNextHop(dst)
	if peer == nil || !peer.isRunning.Load() {
		return false
	}
	peer.StagePackets(elems)
	peer.SendStagedPackets()
	return true
}

// forwardResult is what deliverOrForward decided for one proto=0 packet.
type forwardResult int

const (
	fwdDrop    forwardResult = iota // already counted under its stat
	fwdLocal                        // for this node: deliver to the src tun
	fwdForward                      // for another node: re-send via next hop
)

// deliverOrForward is the decision half of the kernel's
// melnode_routing_deliver_or_forward for one decrypted proto=0 packet
// (routing header + IP packet, still padded). It trims the padding, then
// decides, in the kernel's order: not our IP -> rx_bad_packet; dst is us ->
// local delivery; TTL spent -> rx_ttl_expired; no route/link -> rx_no_route;
// else decrement the TTL and forward. The returned packet is the trimmed one,
// and for fwdForward the next-hop peer to send it to. Actually delivering or
// staging is the caller's job, since that is where batching happens.
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
