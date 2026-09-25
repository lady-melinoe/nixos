package main

import "melnode/dpproto"

// linkLocalTTL is stamped on proto=1/2, which are never forwarded.
const linkLocalTTL = 1

// Link is one configured [[link]] as the control plane sees it: a
// directly-connected neighbor. The Noise tunnel itself lives in the data
// plane (it is created there by dpproto's LinkAdd); what lives here is the
// protocol state that runs over it -- the liveness session (linkmonitor.go)
// and, via path-vector (pathvector.go), the neighbor relationship.
//
// Links are created once at startup from config and outlive any one data
// plane session; each session (re)starts their monitors (session.go).
type Link struct {
	node *Node
	id   uint32

	pubkey       [32]byte
	endpoint     string // configured ip:port literal; "" means listen-only
	prependCount uint32 // AS-prepending for path-vector traffic engineering, see pathvector.go's forwardPath

	monitor *LinkMonitor
}

func newLink(node *Node, id uint32, pubkey [32]byte, endpoint string, prependCount uint32) *Link {
	l := &Link{node: node, id: id, pubkey: pubkey, endpoint: endpoint, prependCount: prependCount}
	l.monitor = newLinkMonitor(l)
	return l
}

func (l *Link) String() string { return "link(" + itoa(l.id) + ")" }

// send hands a control packet to the data plane to transmit on this link.
// Best-effort, like a NIC transmit: with no data plane attached (or a full
// queue) the packet is simply lost, which liveness and path-vector both
// tolerate by design.
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
