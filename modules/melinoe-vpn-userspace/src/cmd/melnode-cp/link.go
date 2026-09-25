package main

import "melnode/dpproto"

const linkLocalTTL = 1

type Link struct {
	node *Node
	id   uint32

	pubkey       [32]byte
	endpoint     string
	prependCount uint32

	monitor *LinkMonitor
}

func newLink(node *Node, id uint32, pubkey [32]byte, endpoint string, prependCount uint32) *Link {
	l := &Link{node: node, id: id, pubkey: pubkey, endpoint: endpoint, prependCount: prependCount}
	l.monitor = newLinkMonitor(l)
	return l
}

func (l *Link) String() string { return "link(" + itoa(l.id) + ")" }

func (l *Link) send(proto, ttl uint8, payload []byte) {
	cl := l.node.dp()
	if cl == nil {
		return
	}
	err := cl.Inject(dpproto.Inject{Link: l.id, Proto: proto, Dst: uint8(l.id), TTL: ttl, Payload: payload})
	if err != nil {
		l.node.log.Verbosef("%v - inject failed: %v", l, err)
	}
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b [10]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
