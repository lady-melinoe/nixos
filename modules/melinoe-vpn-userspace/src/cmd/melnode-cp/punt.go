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
		n.log.Verbosef("proto-%d packet from %v: unknown protocol, dropping", p.Proto, link)
	}
}
