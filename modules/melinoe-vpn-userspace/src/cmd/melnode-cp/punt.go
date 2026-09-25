package main

import (
	"encoding/binary"

	"melnode/dpproto"
)

// handlePunt is the control plane's receive path: the data plane hands up
// every packet whose proto in the 4-byte routing header is non-zero, tagged
// with the link it arrived on, and this dispatches it by proto. (This is
// what used to be a switch inside the data plane's receive loop; the data
// plane no longer knows what these protocols are.)
//
// Payload is everything after the routing header, still including whatever
// padding the sender's encryption added, so each protocol trims itself by its
// own Length field.
func (n *Node) handlePunt(p dpproto.Punt) {
	link := n.links[p.Ingress]
	if link == nil {
		n.log.Verbosef("punt: proto-%d packet from unconfigured link %d, dropping", p.Proto, p.Ingress)
		return
	}
	payload := p.Payload
	if len(payload) < 1 {
		return
	}
	switch p.Proto {
	case livenessProto:
		// BFD-like link liveness (linkmonitor.go). Link-local: never
		// forwarded. The Vers byte selects the payload layout, the same
		// role IP's version nibble plays for proto 0; only Vers=1 exists.
		switch payload[0] {
		case livenessVers1:
			if len(payload) < 6 {
				return
			}
			length := binary.BigEndian.Uint16(payload[4:6])
			if int(length) > len(payload) || int(length) < livenessV1Size {
				return
			}
			link.monitor.handlePacket(payload[:length])
		default:
			n.log.Verbosef("proto-1 packet with unknown version %d from %v", payload[0], link)
		}
	case pvProto:
		// Path-vector routing (pathvector.go). Same Vers-selects-layout
		// convention, but here the Length field does real self-description
		// work (route updates are naturally variable-length).
		switch payload[0] {
		case pvVers1:
			if len(payload) < 4 {
				return
			}
			length := binary.BigEndian.Uint16(payload[2:4])
			if int(length) > len(payload) || int(length) < pvHeaderSize {
				return
			}
			n.pathVector.handlePacket(link, payload[:length])
		default:
			n.log.Verbosef("proto-2 packet with unknown version %d from %v", payload[0], link)
		}
	default:
		// Unknown protos (future control-plane use) are dropped rather than
		// misrouted.
		n.log.Verbosef("proto-%d packet from %v: unknown protocol, dropping", p.Proto, link)
	}
}
