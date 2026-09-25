package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"melnode/dpproto"
)

// ---- the reconciler programs the data plane ----------------------------------------

// Whatever path-vector wants as a next hop ends up in the data plane's table.
func TestReconcileProgramsDataPlaneRoutes(t *testing.T) {
	n := newPVNet(t, 1, 2, 3)
	n.linkUp(1, 2)
	n.linkUp(2, 3)

	h := n.host(1)
	want := map[uint32]uint32{2: 2, 3: 2} // node 3 is reached through node 2
	if len(h.dpRoutes) != len(want) {
		t.Fatalf("data plane routes = %v, want %v", h.dpRoutes, want)
	}
	for d, nh := range want {
		if h.dpRoutes[d] != nh {
			t.Fatalf("data plane routes = %v, want %v", h.dpRoutes, want)
		}
	}
	for d, nh := range want {
		if got, ok := n.nextHop(1, d); !ok || got != nh {
			t.Fatalf("desired next hop to %d = %d,%v want %d", d, got, ok, nh)
		}
	}

	n.linkDown(2, 3)
	if _, ok := h.dpRoutes[3]; ok {
		t.Fatalf("route to node 3 not removed from the data plane: %v", h.dpRoutes)
	}
}

// After attaching, inherited state is not removed until the hold ends: a
// control plane restart must not tear down tuns the data plane is still
// forwarding on while path-vector re-learns the mesh. Additions still happen.
func TestAttachHoldDefersRemovals(t *testing.T) {
	n := newPVNet(t, 1, 2)
	r := n.nodes[1].node.router
	h := n.host(1)

	// Inherited from "before the restart": a tun, a data plane next hop and a
	// kernel prefix route, none of which path-vector knows about yet.
	stray := mustPrefix(t, "10.9.9.9/32")
	h.tuns[7] = true
	h.dpRoutes = map[uint32]uint32{7: 7}
	h.routes[stray] = "t-7"

	r.holdUntil.Store(time.Now().Add(time.Hour).UnixNano())
	r.reconcileOnce()
	if !h.tuns[7] || h.dpRoutes[7] != 7 || h.routes[stray] != "t-7" {
		t.Fatalf("inherited state removed during hold: tuns=%v dp=%v routes=%v", h.tuns, h.dpRoutes, h.routes)
	}

	// New knowledge is applied immediately even during the hold.
	p := mustPrefix(t, "10.9.0.5/32")
	n.nodes[2].AdvertisePrefix(p)
	n.linkUp(1, 2)
	if !h.tuns[2] || h.dpRoutes[2] != 2 || h.routes[p] != "t-2" {
		t.Fatalf("additions were held back: tuns=%v dp=%v routes=%v", h.tuns, h.dpRoutes, h.routes)
	}
	if !h.tuns[7] {
		t.Fatal("inherited tun removed during hold")
	}

	// Hold over: what's still unwanted goes.
	r.holdUntil.Store(0)
	r.reconcileOnce()
	if h.tuns[7] || len(h.dpRoutes) != 1 || h.dpRoutes[2] != 2 {
		t.Fatalf("stale state kept after hold: tuns=%v dp=%v", h.tuns, h.dpRoutes)
	}
	if _, ok := h.routes[stray]; ok {
		t.Fatal("stale kernel route kept after hold")
	}
}

// ---- the punt path -----------------------------------------------------------------

func livenessPunt(link uint32, st linkState, padding int) dpproto.Punt {
	pkt := livenessPacket{
		State:           st,
		DetectMult:      defaultDetectMult,
		MyDiscriminator: 0xabcd,
		DesiredMinTX:    defaultDesiredMinTX,
		RequiredMinRX:   defaultRequiredMinRX,
	}
	payload := append(pkt.encode(), make([]byte, padding)...) // the sender's encryption pads
	return dpproto.Punt{Ingress: link, Proto: livenessProto, Src: uint8(link), Dst: 1, TTL: linkLocalTTL, Payload: payload}
}

func newTestNode(t *testing.T) *Node {
	t.Helper()
	n := &Node{localID: 1, log: &Logger{DiscardLogf, DiscardLogf}, links: map[uint32]*Link{}}
	n.mtu.Store(defaultMTU)
	n.router = newRouter(n, 1, nil, "", "")
	n.router.host = newFakeHost()
	n.pathVector = newPathVector(n, 1)
	return n
}

func TestHandlePuntDispatch(t *testing.T) {
	n := newTestNode(t)
	l := newLink(n, 2, [32]byte{2}, "", 0)
	n.links[2] = l
	l.monitor.Start()
	defer l.monitor.Stop()

	// A padded liveness packet from a peer that is Down moves us Down -> Init.
	n.handlePunt(livenessPunt(2, linkStateDown, 12))
	if got := l.monitor.State(); got != linkStateInit {
		t.Fatalf("state after liveness punt = %v, want Init", got)
	}

	// Garbage of every kind must be dropped without panicking or moving state.
	for _, p := range []dpproto.Punt{
		{Ingress: 2, Proto: livenessProto},                                                            // empty
		{Ingress: 2, Proto: livenessProto, Payload: []byte{livenessVers1}},                            // truncated
		{Ingress: 2, Proto: livenessProto, Payload: []byte{9, 0, 0, 0, 0, 24}},                        // unknown version
		{Ingress: 2, Proto: livenessProto, Payload: []byte{livenessVers1, 0, 0, 0, 0xff, 0xff, 0, 0}}, // length beyond payload
		{Ingress: 2, Proto: pvProto, Payload: []byte{pvVers1, 0, 0}},                                  // truncated path-vector
		{Ingress: 2, Proto: 77, Payload: []byte{1, 2, 3, 4}},                                          // unknown proto
		{Ingress: 99, Proto: livenessProto, Payload: []byte{1, 2, 3, 4, 5, 6, 7, 8}},                  // unconfigured link
	} {
		n.handlePunt(p)
	}
	if got := l.monitor.State(); got != linkStateInit {
		t.Fatalf("state after garbage = %v, want Init", got)
	}
}

// A liveness punt that arrives while the monitor isn't running (before the
// session's monitors start, or after they stop) is ignored, not a crash.
func TestPuntToStoppedMonitorIsIgnored(t *testing.T) {
	n := newTestNode(t)
	l := newLink(n, 2, [32]byte{2}, "", 0)
	n.links[2] = l
	n.handlePunt(livenessPunt(2, linkStateDown, 0)) // never started
	l.monitor.Start()
	l.monitor.Stop()
	l.monitor.Stop() // idempotent
	n.handlePunt(livenessPunt(2, linkStateDown, 0))
	l.monitor.Start() // restartable
	l.monitor.Stop()
}

// ---- sessions ---------------------------------------------------------------------

// fakeDP is a data plane stand-in that records what the control plane asks of it.
type fakeDP struct {
	mu      sync.Mutex
	id      uint32
	links   map[uint32]dpproto.LinkAdd
	injects chan dpproto.Inject
	adds    []dpproto.LinkAdd
	dels    []uint32
	dev     *dpproto.DeviceSet
	// deviceDels counts DeviceDel requests.
	deviceDels int
}

func newFakeDP(id uint32) *fakeDP {
	return &fakeDP{id: id, links: map[uint32]dpproto.LinkAdd{}, injects: make(chan dpproto.Inject, 256)}
}

func (f *fakeDP) Hello(dpproto.Hello) (dpproto.HelloReply, error) {
	return dpproto.HelloReply{Version: dpproto.Version, PID: uint32(os.Getpid()), Configured: f.dev != nil}, nil
}
func (f *fakeDP) DeviceSet(m dpproto.DeviceSet) (dpproto.DeviceSetReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dev != nil && *f.dev != m {
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeExists, "configured differently")
	}
	f.dev = &m
	return dpproto.DeviceSetReply{PubKey: [32]byte{0xaa}}, nil
}
func (f *fakeDP) Stats() ([]dpproto.Stat, error) { return nil, nil }
func (f *fakeDP) DeviceDel() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deviceDels++
	f.dev = nil
	return nil
}
func (f *fakeDP) Quit() {}
func (f *fakeDP) LinkAdd(m dpproto.LinkAdd) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if old, ok := f.links[m.PeerID]; ok && old.PubKey != m.PubKey {
		return dpproto.Errorf(dpproto.CodeExists, "link %d exists with another key", m.PeerID)
	}
	for id, l := range f.links { // like both real data planes: one peerid per key
		if id != m.PeerID && l.PubKey == m.PubKey {
			return dpproto.Errorf(dpproto.CodeExists, "public key already used by link %d", id)
		}
	}
	f.links[m.PeerID] = m
	f.adds = append(f.adds, m)
	return nil
}
func (f *fakeDP) LinkDel(id uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.links[id]; !ok {
		return dpproto.Errorf(dpproto.CodeNotFound, "no link %d", id)
	}
	delete(f.links, id)
	f.dels = append(f.dels, id)
	return nil
}
func (f *fakeDP) LinkList() ([]dpproto.LinkInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []dpproto.LinkInfo
	for id, l := range f.links {
		out = append(out, dpproto.LinkInfo{PeerID: id, PubKey: l.PubKey})
	}
	return out, nil
}
func (f *fakeDP) TunCreate(dpproto.TunCreate) (dpproto.TunCreateReply, error) {
	return dpproto.TunCreateReply{}, dpproto.Errorf(dpproto.CodeUnsupported, "fake")
}
func (f *fakeDP) TunStart(uint32) error               { return nil }
func (f *fakeDP) TunDestroy(uint32) error             { return nil }
func (f *fakeDP) TunList() ([]dpproto.TunInfo, error) { return nil, nil }
func (f *fakeDP) RouteSet(dpproto.Route) error        { return nil }
func (f *fakeDP) RouteDel(uint32) error               { return nil }
func (f *fakeDP) RouteList() ([]dpproto.Route, error) { return nil, nil }
func (f *fakeDP) Inject(m dpproto.Inject)             { f.injects <- m }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// One full session: the configured links are pushed to the data plane (and
// ones it shouldn't have are removed, and a changed key replaced), the MTU is
// learned, liveness flows both ways over the socket, and a deliberate stop
// sends AdminDown on every link.
func TestSessionSyncsLinksAndCarriesLiveness(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dp.sock")
	dp := newFakeDP(1)
	// Pre-existing state on the data plane: link 9 is stale, link 2 has an old key.
	dp.links[9] = dpproto.LinkAdd{PeerID: 9, PubKey: [32]byte{9}}
	dp.links[2] = dpproto.LinkAdd{PeerID: 2, PubKey: [32]byte{0xee}}

	srv, err := dpproto.NewServer(sock, dp)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Run()

	n := newTestNode(t)
	n.device = dpproto.DeviceSet{LocalID: 1, MTU: 1300, ListenPort: 60198}
	n.links[2] = newLink(n, 2, [32]byte{2}, "10.0.0.2:60198", 0)
	n.links[3] = newLink(n, 3, [32]byte{3}, "", 0)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); n.runSessions(sock, stop) }()

	waitFor(t, "attach", func() bool { return n.dp() != nil })
	if p := n.pubkey.Load(); p == nil || *p != [32]byte{0xaa} {
		t.Fatalf("pubkey = %v, want the one DeviceSet returned", p)
	}
	waitFor(t, "links pushed", func() bool {
		dp.mu.Lock()
		defer dp.mu.Unlock()
		return len(dp.links) == 2 && dp.links[2].PubKey == [32]byte{2} && dp.links[2].Endpoint == "10.0.0.2:60198" && dp.links[3].PubKey == [32]byte{3}
	})
	dp.mu.Lock()
	staleGone := dp.links[9].PeerID == 0
	dp.mu.Unlock()
	if !staleGone {
		t.Fatal("stale link 9 not removed")
	}

	// The monitors are running: they inject liveness onto their links.
	seen := map[uint32]bool{}
	waitFor(t, "liveness injected on both links", func() bool {
		for {
			select {
			case in := <-dp.injects:
				if in.Proto == livenessProto {
					if in.TTL != linkLocalTTL || in.Dst != uint8(in.Link) {
						t.Errorf("bad inject header: %+v", in)
					}
					seen[in.Link] = true
				}
			default:
				return seen[2] && seen[3]
			}
		}
	})

	// A liveness punt from the data plane reaches the right monitor.
	srv.Punt(livenessPunt(2, linkStateDown, 8))
	waitFor(t, "punt delivered to link 2's monitor", func() bool { return n.links[2].monitor.State() == linkStateInit })

	// Deliberate stop: AdminDown goes out on every link, and the data plane's
	// links are left alone.
	close(stop)
	<-done
	got := map[uint32]bool{}
	for len(got) < 2 {
		select {
		case in := <-dp.injects:
			if in.Proto == livenessProto && len(in.Payload) > 2 && linkState(in.Payload[2]) == linkStateAdminDown {
				got[in.Link] = true
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no AdminDown on links after stop, saw %v", got)
		}
	}
	dp.mu.Lock()
	nLinks := len(dp.links)
	dp.mu.Unlock()
	if nLinks != 2 {
		t.Fatalf("shutdown touched the data plane's links (%d left, want 2)", nLinks)
	}
	if n.dp() != nil {
		t.Fatal("still attached after stop")
	}
}

// If the data plane goes away the control plane notices, takes its links down
// (so path-vector withdraws everything) and re-attaches when it returns.
func TestSessionSurvivesDataPlaneRestart(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dp.sock")
	start := func() *dpproto.Server {
		srv, err := dpproto.NewServer(sock, newFakeDP(1))
		if err != nil {
			t.Fatal(err)
		}
		go srv.Run()
		return srv
	}
	srv := start()

	n := newTestNode(t)
	l := newLink(n, 2, [32]byte{2}, "", 0)
	n.links[2] = l
	var mu sync.Mutex
	var events []linkState
	l.monitor.SetOnStateChange(func(_ *Link, s linkState) { mu.Lock(); events = append(events, s); mu.Unlock() })
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); n.runSessions(sock, stop) }()
	defer func() { close(stop); <-done }()

	waitFor(t, "first attach", func() bool { return n.dp() != nil })
	first := n.dp()
	srv.Close()
	waitFor(t, "detach after data plane loss", func() bool { return n.dp() == nil })
	mu.Lock()
	sawAdminDown := len(events) > 0 && events[len(events)-1] == linkStateAdminDown
	mu.Unlock()
	if !sawAdminDown {
		t.Fatalf("link not taken down on session loss: %v", events)
	}

	srv2 := start()
	defer srv2.Close()
	waitFor(t, "re-attach", func() bool { c := n.dp(); return c != nil && c != first })
}

// A data plane configured differently than we want can't be changed live: the
// attach fails and has it tear its device down, so the next attempt configures
// it afresh (no process restart needed).
func TestAttachReplacesMisconfiguredDataplane(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dp.sock")
	dp := newFakeDP(1)
	dp.dev = &dpproto.DeviceSet{LocalID: 1, MTU: 1200}
	srv, err := dpproto.NewServer(sock, dp)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Run()

	n := newTestNode(t)
	n.device = dpproto.DeviceSet{LocalID: 1, MTU: 1416}
	if _, err := n.attach(sock); err == nil {
		t.Fatal("attach to a differently-configured data plane succeeded")
	}
	waitFor(t, "DeviceDel request", func() bool {
		dp.mu.Lock()
		defer dp.mu.Unlock()
		return dp.deviceDels == 1
	})
}

func TestWrongBinary(t *testing.T) {
	n := &Node{}
	if n.wrongBinary(uint32(os.Getpid())) {
		t.Fatal("no dpCommand: nothing is wrong")
	}
	n.dpCommand = []string{os.Args[0]}
	if n.wrongBinary(uint32(os.Getpid())) {
		t.Fatal("same binary flagged as wrong")
	}
	n.dpCommand = []string{"/bin/sh"}
	if !n.wrongBinary(uint32(os.Getpid())) {
		t.Fatal("different binary not flagged")
	}
}
