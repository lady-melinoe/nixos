package main

import (
	"encoding/binary"

	"melnode/dpproto"
)

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
		n.log.Verbosef("proto-1 packet from %v reached the control plane; the data plane handles liveness", link)
	case pvProto:
		switch payload[0] {
		case pvVers2:
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
		n.log.Verbosef("proto-%d packet from %v: unknown protocol, dropping", p.Proto, link)
	}
}
