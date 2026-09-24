package main

import "testing"

// pvNet is an in-memory mesh of real PathVector instances. Messages go
// through encodePVPacket/decodePVPacket (so the wire codec is exercised
// too) and are delivered FIFO by drain(), which avoids the re-entrant
// locking a synchronous delivery would hit.
type pvNet struct {
	t     *testing.T
	nodes map[uint32]*PathVector
	peers map[[2]uint32]*Peer // [owner, neighborID] -> owner's Peer for that neighbor
	up    map[[2]uint32]bool  // undirected, key sorted
	queue []pvMsg
}

type pvMsg struct {
	from, to  uint32
	announces []pvAnnouncement
	withdraws []uint32
}

func linkKey(a, b uint32) [2]uint32 {
	if a > b {
		a, b = b, a
	}
	return [2]uint32{a, b}
}

func newPVNet(t *testing.T, ids ...uint32) *pvNet {
	n := &pvNet{t: t, nodes: map[uint32]*PathVector{}, peers: map[[2]uint32]*Peer{}, up: map[[2]uint32]bool{}}
	for _, id := range ids {
		id := id
		dev := &Device{localID: id, mtu: defaultMTU, log: &Logger{DiscardLogf, DiscardLogf}}
		dev.router = newRouter(dev, id, "t-", nil, "", "")
		dev.router.noTuns = true
		pv := newPathVector(dev, id)
		pv.sendHook = func(peer *Peer, a []pvAnnouncement, w []uint32) {
			n.queue = append(n.queue, pvMsg{from: id, to: peer.id, announces: a, withdraws: w})
		}
		n.nodes[id] = pv
	}
	return n
}

func (n *pvNet) peer(owner, nbr uint32) *Peer {
	k := [2]uint32{owner, nbr}
	if p, ok := n.peers[k]; ok {
		return p
	}
	p := &Peer{id: nbr}
	n.peers[k] = p
	return p
}

func (n *pvNet) linkUp(a, b uint32) {
	n.up[linkKey(a, b)] = true
	n.nodes[a].OnLinkUp(n.peer(a, b))
	n.nodes[b].OnLinkUp(n.peer(b, a))
	n.drain()
}

func (n *pvNet) linkDown(a, b uint32) {
	delete(n.up, linkKey(a, b))
	n.nodes[a].OnLinkDown(n.peer(a, b))
	n.nodes[b].OnLinkDown(n.peer(b, a))
	n.drain()
}

func (n *pvNet) drain() {
	for steps := 0; len(n.queue) > 0; steps++ {
		if steps > 10000 {
			n.t.Fatal("route updates never settled (announce storm)")
		}
		m := n.queue[0]
		n.queue = n.queue[1:]
		if !n.up[linkKey(m.from, m.to)] {
			continue // link is down, packet lost
		}
		wire := encodePVPacket(m.announces, m.withdraws)
		if _, _, ok := decodePVPacket(wire); !ok {
			n.t.Fatalf("codec round-trip failed for %d->%d", m.from, m.to)
		}
		n.nodes[m.to].handlePacket(n.peer(m.to, m.from), wire)
	}
}

func (n *pvNet) fullSyncAll() {
	for _, pv := range n.nodes {
		pv.mu.Lock()
		nb := pv.neighborsSnapshotLocked()
		pv.mu.Unlock()
		for _, p := range nb {
			pv.fullSyncTo(p)
		}
	}
	n.drain()
}

// nextHop returns node's currently-installed next hop to dest.
func (n *pvNet) nextHop(node, dest uint32) (uint32, bool) {
	return n.nodes[node].dev.router.LookupRoute(dest)
}

// assertNoLoop follows next hops from src toward dest and fails on a
// loop or a dead end that isn't dest.
func (n *pvNet) route(src, dest uint32) (path []uint32, ok bool) {
	cur := src
	seen := map[uint32]bool{}
	for cur != dest {
		if seen[cur] {
			return append(path, cur), false // loop
		}
		seen[cur] = true
		path = append(path, cur)
		nh, has := n.nextHop(cur, dest)
		if !has || !n.up[linkKey(cur, nh)] {
			return path, false
		}
		cur = nh
	}
	return append(path, cur), true
}

// TestSplitHorizonStaleRoute reproduces the double-failure scenario:
//
//	D--B, D--C, C--A, A--B     (ids: A=1, C=2, B=3, D=4)
//
// A's best to D is via C (lowest neighbor id on a tie), with B as a
// non-best alternative. B's direct D link dies; B's new best is via A,
// and split-horizon must withdraw B's earlier advertisement from A
// rather than silently skipping it. Then C-D dies: D is now genuinely
// unreachable, so nobody may keep a route to it.
func TestSplitHorizonStaleRoute(t *testing.T) {
	const A, C, B, D = 1, 2, 3, 4
	n := newPVNet(t, A, B, C, D)
	n.linkUp(D, B)
	n.linkUp(D, C)
	n.linkUp(C, A)
	n.linkUp(A, B)
	n.fullSyncAll()

	if nh, _ := n.nextHop(A, D); nh != C {
		t.Fatalf("setup: A should reach D via C, got nh=%d", nh)
	}

	n.linkDown(D, B)
	n.fullSyncAll()
	if p, ok := n.route(B, D); !ok {
		t.Fatalf("after B-D failure B should still reach D via A/C, got %v ok=%v", p, ok)
	}

	n.linkDown(D, C) // D is now completely cut off
	n.fullSyncAll()  // periodic resync must not resurrect anything either

	for _, node := range []uint32{A, B, C} {
		if nh, has := n.nextHop(node, D); has {
			t.Errorf("node %d still has a route to unreachable D (via %d): stale learned entry survived", node, nh)
		}
	}
}

// TestPathLengthOverflow: with heavy prepending a forwarded path can
// exceed 255 hops, which used to wrap byte(len(path)) and corrupt the
// packet. It must now be withheld (not advertised) instead.
func TestPathLengthOverflow(t *testing.T) {
	n := newPVNet(t, 1, 2, 3)
	n.peer(2, 3).prependCount = 254 // 2 relaying toward 3 adds 255 copies of itself
	n.linkUp(1, 2)
	n.linkUp(2, 3)
	n.fullSyncAll()

	if _, ok := n.nextHop(2, 1); !ok {
		t.Fatal("2 should reach 1")
	}
	if nh, ok := n.nextHop(3, 1); ok {
		t.Fatalf("3 should not get a route to 1 (path would be %d+ hops), got nh=%d", 256, nh)
	}
	// and the direct route to 2 is unaffected
	if _, ok := n.nextHop(3, 2); !ok {
		t.Fatal("3 should still reach 2")
	}
}

// TestChunking: a full table bigger than one MTU-sized packet must be
// split across packets (each <= the MTU) and still arrive completely.
func TestChunking(t *testing.T) {
	n := newPVNet(t, 1, 2)
	for _, pv := range n.nodes {
		pv.dev.mtu = 200 // tiny, to force many chunks with few routes
	}
	// node 1 claims lots of prefixes -> a big self-origin entry per packet limit
	for i := 0; i < 30; i++ { // 8+2+1+1+30*5 = 162 <= 200
		n.nodes[1].AdvertisePrefix(pvPrefix{addr: uint32(0x0a000000 + i), len: 32})
	}
	n.linkUp(1, 2)
	n.fullSyncAll()
	if got := len(n.nodes[2].prefixClaims); got != 30 {
		t.Fatalf("node 2 learned %d prefixes, want 30", got)
	}

	// many dests behind node 1 -> node 2's table to node 3 must split
	m := newPVNet(t, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12)
	for _, pv := range m.nodes {
		pv.dev.mtu = 40 // 8 hdr + ~5 entries of 6 bytes
	}
	sizes := 0
	orig := m.nodes[2].sendHook
	m.nodes[2].sendHook = func(p *Peer, a []pvAnnouncement, w []uint32) {
		if sz := len(encodePVPacket(a, w)); sz > 40 {
			t.Errorf("packet of %d bytes exceeds mtu 40", sz)
		}
		sizes++
		orig(p, a, w)
	}
	for id := uint32(3); id <= 12; id++ {
		m.linkUp(2, id)
	}
	m.linkUp(1, 2)
	m.fullSyncAll()
	for id := uint32(3); id <= 12; id++ {
		if _, ok := m.nextHop(1, id); !ok {
			t.Errorf("node 1 missing route to %d after chunked sync", id)
		}
	}
	if sizes < 3 {
		t.Errorf("expected several chunks from node 2, saw %d packets", sizes)
	}
}

// TestResyncKernelRestoresRoutes checks the self-heal path: if a peerid
// route (and its tun) ever goes missing while path-vector still considers
// the dest reachable -- e.g. a tun that failed to come up -- the periodic
// resync puts it back without needing another topology change.
func TestResyncKernelRestoresRoutes(t *testing.T) {
	n := newPVNet(t, 1, 2)
	n.linkUp(1, 2)
	r := n.nodes[1].dev.router
	if _, ok := r.LookupRoute(2); !ok {
		t.Fatal("no route to node 2 after link up")
	}

	r.RemoveRoute(2) // simulate the lost route/tun
	if _, ok := r.LookupRoute(2); ok {
		t.Fatal("route to node 2 still present after RemoveRoute")
	}

	n.nodes[1].resyncKernel()
	if nh, ok := r.LookupRoute(2); !ok || nh != 2 {
		t.Fatalf("resyncKernel did not restore the route to node 2 (nh=%d ok=%v)", nh, ok)
	}
}
