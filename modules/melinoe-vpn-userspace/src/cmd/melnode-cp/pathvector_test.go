package main

import (
	"fmt"
	"sort"
	"sync"
	"testing"
)

// pvNet is an in-memory mesh of real PathVector instances. Messages go
// through encodePVPacket/decodePVPacket (so the wire codec is exercised
// too) and are delivered FIFO by drain(), which avoids the re-entrant
// locking a synchronous delivery would hit.
type pvNet struct {
	t     *testing.T
	nodes map[uint32]*PathVector
	peers map[[2]uint32]*Link // [owner, neighborID] -> owner's Peer for that neighbor
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
	n := &pvNet{t: t, nodes: map[uint32]*PathVector{}, peers: map[[2]uint32]*Link{}, up: map[[2]uint32]bool{}}
	for _, id := range ids {
		id := id
		node := &Node{localID: id, log: &Logger{DiscardLogf, DiscardLogf}}
		node.mtu.Store(defaultMTU)
		node.router = newRouter(node, id, nil, "", "")
		node.router.host = newFakeHost()
		pv := newPathVector(node, id)
		pv.sendHook = func(peer *Link, a []pvAnnouncement, w []uint32) {
			n.queue = append(n.queue, pvMsg{from: id, to: peer.id, announces: a, withdraws: w})
		}
		n.nodes[id] = pv
	}
	return n
}

func (n *pvNet) peer(owner, nbr uint32) *Link {
	k := [2]uint32{owner, nbr}
	if p, ok := n.peers[k]; ok {
		return p
	}
	p := &Link{id: nbr}
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
	n.reconcileAll()
}

// reconcileAll runs one reconcile pass on every node (there's no background
// loop in tests since Start isn't called).
func (n *pvNet) reconcileAll() {
	for _, pv := range n.nodes {
		pv.node.router.reconcileOnce()
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
	return n.nodes[node].node.router.LookupRoute(dest)
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
		pv.node.mtu.Store(200) // tiny, to force many chunks with few routes
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
		pv.node.mtu.Store(40) // 8 hdr + ~5 entries of 6 bytes
	}
	sizes := 0
	orig := m.nodes[2].sendHook
	m.nodes[2].sendHook = func(p *Link, a []pvAnnouncement, w []uint32) {
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

// fakeHost is an in-memory hostOps: tuns and proto-198 routes as plain maps.
type fakeHost struct {
	mu         sync.Mutex
	tuns       map[uint32]bool
	routes     map[pvPrefix]string
	dpRoutes   map[uint32]uint32 // what the data plane's next-hop table was last programmed to
	failCreate map[uint32]int    // remaining EnsureTun failures per peerid
	ops        []string          // ordered log of mutating calls
}

func newFakeHost() *fakeHost {
	return &fakeHost{tuns: map[uint32]bool{}, routes: map[pvPrefix]string{}, failCreate: map[uint32]int{}}
}

func (f *fakeHost) Ready() bool { return true }

func (f *fakeHost) Tuns() ([]uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uint32
	for id := range f.tuns {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// ProgramRoutes stands in for the data plane's next-hop table.
func (f *fakeHost) ProgramRoutes(want map[uint32]uint32, prune bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dpRoutes == nil || prune {
		f.dpRoutes = make(map[uint32]uint32, len(want))
	}
	for d, nh := range want {
		f.dpRoutes[d] = nh
	}
	return nil
}

func (f *fakeHost) TunName(id uint32) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.tuns[id] {
		return "", false
	}
	return fmt.Sprintf("t-%d", id), true
}

func (f *fakeHost) EnsureTun(id uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate[id] > 0 {
		f.failCreate[id]--
		return fmt.Errorf("injected tun create failure")
	}
	f.tuns[id] = true
	f.ops = append(f.ops, fmt.Sprintf("tun+%d", id))
	return nil
}

func (f *fakeHost) DestroyTun(id uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tuns, id)
	// like the kernel: routes on a deleted device vanish with it
	for p, dev := range f.routes {
		if dev == fmt.Sprintf("t-%d", id) {
			delete(f.routes, p)
		}
	}
	f.ops = append(f.ops, fmt.Sprintf("tun-%d", id))
}

func (f *fakeHost) ListRoutes() (map[pvPrefix]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[pvPrefix]string, len(f.routes))
	for p, d := range f.routes {
		out[p] = d
	}
	return out, nil
}

func (f *fakeHost) ReplaceRoute(p pvPrefix, dev string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[p] = dev
	f.ops = append(f.ops, fmt.Sprintf("route+%v", p))
	return nil
}

func (f *fakeHost) DelRoute(p pvPrefix) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.routes, p)
	f.ops = append(f.ops, fmt.Sprintf("route-%v", p))
	return nil
}

func (n *pvNet) host(node uint32) *fakeHost {
	return n.nodes[node].node.router.host.(*fakeHost)
}

func mustPrefix(t *testing.T, s string) pvPrefix {
	p, ok := parsePrefix(s)
	if !ok {
		t.Fatalf("bad prefix %q", s)
	}
	return p
}

// A dest's tun and a route for a prefix it owns appear once it's reachable,
// and go away again when the link drops.
func TestReconcileInstallsAndRemoves(t *testing.T) {
	n := newPVNet(t, 1, 2)
	p := mustPrefix(t, "10.9.0.5/32")
	n.nodes[2].AdvertisePrefix(p)
	n.linkUp(1, 2)

	h := n.host(1)
	if !h.tuns[2] {
		t.Fatal("no tun for node 2 after link up")
	}
	if h.routes[p] != "t-2" {
		t.Fatalf("route for %v = %q, want t-2", p, h.routes[p])
	}
	// the owner itself installs nothing for its own prefix
	if len(n.host(2).routes) != 0 {
		t.Fatalf("owner installed routes for its own prefix: %v", n.host(2).routes)
	}

	n.linkDown(1, 2)
	if h.tuns[2] || len(h.routes) != 0 {
		t.Fatalf("after link down: tuns=%v routes=%v, want none", h.tuns, h.routes)
	}
}

// The original bug: a tun that failed to come up left prefix routes
// permanently missing. Now the pass reports it, and the next one fixes it
// with no new path-vector event needed.
func TestReconcileRetriesFailedTun(t *testing.T) {
	n := newPVNet(t, 1, 2)
	p := mustPrefix(t, "10.9.0.5/32")
	n.nodes[2].AdvertisePrefix(p)
	n.host(1).failCreate[2] = 2
	n.linkUp(1, 2)

	h := n.host(1)
	if h.tuns[2] || len(h.routes) != 0 {
		t.Fatal("tun/route present despite injected create failure")
	}
	r := n.nodes[1].node.router
	if !r.reconcileOnce() {
		t.Fatal("pass with a still-failing tun should ask for a retry")
	}
	if r.reconcileOnce() {
		t.Fatal("pass that fixed everything should not ask for a retry")
	}
	if !h.tuns[2] || h.routes[p] != "t-2" {
		t.Fatalf("not healed: tuns=%v routes=%v", h.tuns, h.routes)
	}
}

// Routes are only installed after the owner's tun exists (tun+ before route+).
func TestReconcileOrdersTunBeforeRoute(t *testing.T) {
	n := newPVNet(t, 1, 2)
	p := mustPrefix(t, "10.9.0.5/32")
	n.nodes[2].AdvertisePrefix(p)
	n.linkUp(1, 2)
	ops := n.host(1).ops
	ti, ri := -1, -1
	for i, o := range ops {
		if o == "tun+2" && ti < 0 {
			ti = i
		}
		if o == "route+"+p.String() && ri < 0 {
			ri = i
		}
	}
	if ti < 0 || ri < 0 || ti > ri {
		t.Fatalf("bad ordering: %v", ops)
	}
}

// Drift is healed: a route someone flushed, a stray proto-198 route nobody
// wants, and a tun that vanished all get put right on the next pass.
func TestReconcileHealsDrift(t *testing.T) {
	n := newPVNet(t, 1, 2)
	p := mustPrefix(t, "10.9.0.5/32")
	stray := mustPrefix(t, "10.9.9.9/32")
	n.nodes[2].AdvertisePrefix(p)
	n.linkUp(1, 2)

	h := n.host(1)
	delete(h.routes, p)
	h.routes[stray] = "t-2"
	r := n.nodes[1].node.router
	r.reconcileOnce()
	if h.routes[p] != "t-2" {
		t.Fatal("flushed route not restored")
	}
	if _, ok := h.routes[stray]; ok {
		t.Fatal("stray route not removed")
	}

	delete(h.tuns, 2)
	delete(h.routes, p)
	r.reconcileOnce()
	if !h.tuns[2] || h.routes[p] != "t-2" {
		t.Fatalf("vanished tun not restored: tuns=%v routes=%v", h.tuns, h.routes)
	}
}

// A prefix that becomes ours drops the route installed for its old owner.
func TestReconcileDropsRouteWhenPrefixBecomesLocal(t *testing.T) {
	n := newPVNet(t, 1, 2)
	p := mustPrefix(t, "10.9.0.5/32")
	n.nodes[2].AdvertisePrefix(p)
	n.linkUp(1, 2)
	h := n.host(1)
	if h.routes[p] == "" {
		t.Fatal("precondition: route via node 2")
	}
	n.nodes[1].AdvertisePrefix(p) // node 1 is now the shorter-path owner
	n.drain()
	if _, ok := h.routes[p]; ok {
		t.Fatalf("stale route kept after prefix became local: %v", h.routes)
	}
}

// A node's own route is announced as path [self] however the announcement
// was triggered; forwardPath (which appends us again) is only for relaying
// other nodes' routes. It used to go out as [self self] when a prefix was
// advertised, and back to [self] at the next full sync.
func TestSelfOriginPathLength(t *testing.T) {
	n := newPVNet(t, 1, 2)
	n.linkUp(1, 2)
	n.nodes[2].AdvertisePrefix(mustPrefix(t, "10.9.0.5/32"))
	n.drain()
	if got := n.nodes[1].best[2].path; len(got) != 1 {
		t.Fatalf("node 1's path to 2 after 2 advertised a prefix = %v, want [2]", got)
	}
}

// Whatever order concurrent changes land in, the last update a neighbor
// gets for each dest must match our current state, without waiting for a
// full sync (proto=2 isn't acked, so a stale last update would stick).
func TestLastUpdateIsCurrent(t *testing.T) {
	n := newPVNet(t, 1)
	pv := n.nodes[1]
	to2, to3 := n.peer(1, 2), n.peer(1, 3)

	type belief struct {
		reachable bool
		prefixes  []pvPrefix
	}
	var mu sync.Mutex
	heard := map[uint32]belief{} // what neighbor 2 currently believes, per dest
	pv.sendHook = func(peer *Link, a []pvAnnouncement, w []uint32) {
		if peer.id != 2 {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, x := range a {
			heard[x.dest] = belief{true, x.prefixes}
		}
		for _, d := range w {
			heard[d] = belief{}
		}
	}
	pv.OnLinkUp(to2)
	pv.OnLinkUp(to3)

	prefixes := []pvPrefix{mustPrefix(t, "10.9.0.1/32"), mustPrefix(t, "10.9.0.2/32"), mustPrefix(t, "10.9.0.3/32")}
	announce9 := encodePVPacket([]pvAnnouncement{{dest: 9, path: []uint32{9, 3}}}, nil)
	withdraw9 := encodePVPacket(nil, []uint32{9})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if p := prefixes[(g+i)%len(prefixes)]; i%2 == 0 {
					pv.AdvertisePrefix(p)
				} else {
					pv.WithdrawPrefix(p)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if (g+i)%2 == 0 {
					pv.handlePacket(to3, announce9)
				} else {
					pv.handlePacket(to3, withdraw9)
				}
			}
		}()
	}
	wg.Wait()

	pv.mu.Lock()
	wantPrefixes := pv.localPrefixesListLocked()
	_, want9 := pv.best[9]
	pv.mu.Unlock()

	mu.Lock()
	defer mu.Unlock()
	if got := heard[1]; !got.reachable || !prefixesEqual(got.prefixes, wantPrefixes) {
		t.Errorf("neighbor 2 last heard node 1's prefixes as %v, currently %v", got.prefixes, wantPrefixes)
	}
	if got := heard[9].reachable; got != want9 {
		t.Errorf("neighbor 2 last heard dest 9 reachable=%v, currently %v", got, want9)
	}
}
