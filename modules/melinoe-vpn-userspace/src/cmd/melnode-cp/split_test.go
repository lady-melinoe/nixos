package main

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"melnode/dpproto"
)

func TestReconcileProgramsDataPlaneRoutes(t *testing.T) {
	n := newPVNet(t, 1, 2, 3)
	n.linkUp(1, 2)
	n.linkUp(2, 3)

	h := n.host(1)
	want := map[uint32]uint32{2: 2, 3: 2}
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

func TestAttachHoldDefersKernelRouteRemoval(t *testing.T) {
	n := newPVNet(t, 1, 2)
	r := n.nodes[1].node.router
	h := n.host(1)

	stray := mustPrefix(t, "10.9.9.9/32")
	h.routes[stray] = "t-7"

	r.holdUntil.Store(time.Now().Add(time.Hour).UnixNano())
	r.reconcileOnce()
	if h.routes[stray] != "t-7" {
		t.Fatalf("inherited kernel route removed during hold: %v", h.routes)
	}

	p := mustPrefix(t, "10.9.0.5/32")
	n.nodes[2].AdvertisePrefix(p)
	n.linkUp(1, 2)
	if !h.tuns[2] || h.dpRoutes[2] != 2 || h.routes[p] != "t-2" {
		t.Fatalf("additions were held back: tuns=%v dp=%v routes=%v", h.tuns, h.dpRoutes, h.routes)
	}
	if h.routes[stray] != "t-7" {
		t.Fatal("inherited kernel route removed during hold")
	}

	r.holdUntil.Store(0)
	r.reconcileOnce()
	if _, ok := h.routes[stray]; ok {
		t.Fatal("stale kernel route kept after hold")
	}
}

func livenessPunt(link uint32, st linkState, padding int) dpproto.Punt {
	pkt := livenessPacket{
		State:           st,
		DetectMult:      defaultDetectMult,
		MyDiscriminator: 0xabcd,
		DesiredMinTX:    defaultDesiredMinTX,
		RequiredMinRX:   defaultRequiredMinRX,
	}
	payload := append(pkt.encode(), make([]byte, padding)...)
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

	n.handlePunt(livenessPunt(2, linkStateDown, 12))
	if got := l.monitor.State(); got != linkStateInit {
		t.Fatalf("state after liveness punt = %v, want Init", got)
	}

	for _, p := range []dpproto.Punt{
		{Ingress: 2, Proto: livenessProto},
		{Ingress: 2, Proto: livenessProto, Payload: []byte{livenessVers1}},
		{Ingress: 2, Proto: livenessProto, Payload: []byte{9, 0, 0, 0, 0, 24}},
		{Ingress: 2, Proto: livenessProto, Payload: []byte{livenessVers1, 0, 0, 0, 0xff, 0xff, 0, 0}},
		{Ingress: 2, Proto: pvProto, Payload: []byte{pvVers1, 0, 0}},
		{Ingress: 2, Proto: 77, Payload: []byte{1, 2, 3, 4}},
		{Ingress: 99, Proto: livenessProto, Payload: []byte{1, 2, 3, 4, 5, 6, 7, 8}},
	} {
		n.handlePunt(p)
	}
	if got := l.monitor.State(); got != linkStateInit {
		t.Fatalf("state after garbage = %v, want Init", got)
	}
}

func TestPuntToStoppedMonitorIsIgnored(t *testing.T) {
	n := newTestNode(t)
	l := newLink(n, 2, [32]byte{2}, "", 0)
	n.links[2] = l
	n.handlePunt(livenessPunt(2, linkStateDown, 0))
	l.monitor.Start()
	l.monitor.Stop()
	l.monitor.Stop()
	n.handlePunt(livenessPunt(2, linkStateDown, 0))
	l.monitor.Start()
	l.monitor.Stop()
}

type fakeDP struct {
	mu      sync.Mutex
	links   map[uint32]dpproto.LinkSet
	injects chan dpproto.Inject
	dev     *dpproto.DeviceSet
	closed  bool
}

func newFakeDP() *fakeDP {
	return &fakeDP{links: map[uint32]dpproto.LinkSet{}, injects: make(chan dpproto.Inject, 256)}
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
func (f *fakeDP) LinkSet(m dpproto.LinkSet) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if old, ok := f.links[m.PeerID]; ok && old.PubKey != m.PubKey {
		return dpproto.Errorf(dpproto.CodeExists, "link %d exists with another key", m.PeerID)
	}
	f.links[m.PeerID] = m
	return nil
}
func (f *fakeDP) LinkDel(id uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.links[id]; !ok {
		return dpproto.Errorf(dpproto.CodeNotFound, "no link %d", id)
	}
	delete(f.links, id)
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
func (f *fakeDP) TunSet(dpproto.TunSet) (dpproto.TunInfo, error) {
	return dpproto.TunInfo{}, dpproto.Errorf(dpproto.CodeUnsupported, "fake")
}
func (f *fakeDP) TunDel(uint32) error                 { return nil }
func (f *fakeDP) TunList() ([]dpproto.TunInfo, error) { return nil, nil }
func (f *fakeDP) RouteSet([]dpproto.Route) error      { return nil }
func (f *fakeDP) RouteList() ([]dpproto.Route, error) { return nil, nil }
func (f *fakeDP) Inject(m dpproto.Inject)             { f.injects <- m }
func (f *fakeDP) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

type fakeServer struct {
	*dpproto.Server
	mu   sync.Mutex
	dps  []*fakeDP
	sess []*dpproto.Session
}

func newFakeServer(t *testing.T, sock string) *fakeServer {
	t.Helper()
	fs := &fakeServer{}
	srv, err := dpproto.NewServer(sock, func(s *dpproto.Session) dpproto.Handler {
		dp := newFakeDP()
		fs.mu.Lock()
		fs.dps = append(fs.dps, dp)
		fs.sess = append(fs.sess, s)
		fs.mu.Unlock()
		return dp
	})
	if err != nil {
		t.Fatal(err)
	}
	fs.Server = srv
	go srv.Run()
	return fs
}

func (fs *fakeServer) last() (*fakeDP, *dpproto.Session) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.dps) == 0 {
		return nil, nil
	}
	return fs.dps[len(fs.dps)-1], fs.sess[len(fs.sess)-1]
}

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

func TestSessionSyncsLinksAndCarriesLiveness(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dp.sock")
	srv := newFakeServer(t, sock)
	defer srv.Close()

	n := newTestNode(t)
	n.device = dpproto.DeviceSet{LocalID: 1, MTU: 1300, ListenPort: 60198}
	n.links[2] = newLink(n, 2, [32]byte{2}, "10.0.0.2:60198", 0)
	n.links[3] = newLink(n, 3, [32]byte{3}, "", 0)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); n.runSessions(sock, stop) }()

	waitFor(t, "attach", func() bool { return n.dp() != nil })
	dp, sess := srv.last()
	if p := n.pubkey.Load(); p == nil || *p != [32]byte{0xaa} {
		t.Fatalf("pubkey = %v, want the one DeviceSet returned", p)
	}
	waitFor(t, "links pushed", func() bool {
		dp.mu.Lock()
		defer dp.mu.Unlock()
		return len(dp.links) == 2 && dp.links[2].PubKey == [32]byte{2} && dp.links[2].Endpoint == "10.0.0.2:60198" && dp.links[3].PubKey == [32]byte{3}
	})

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

	sess.Punt(livenessPunt(2, linkStateDown, 8))
	waitFor(t, "punt delivered to link 2's monitor", func() bool { return n.links[2].monitor.State() == linkStateInit })

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
	if n.dp() != nil {
		t.Fatal("still attached after stop")
	}
	waitFor(t, "data plane instance closed", func() bool {
		dp.mu.Lock()
		defer dp.mu.Unlock()
		return dp.closed
	})
}

func TestSessionSurvivesDataPlaneRestart(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dp.sock")
	start := func() *fakeServer { return newFakeServer(t, sock) }
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
