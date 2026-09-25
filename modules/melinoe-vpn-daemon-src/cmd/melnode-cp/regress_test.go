package main

import (
	"path/filepath"
	"testing"

	"melnode/dpproto"
)

// A tun that already exists when we attach (adopted after a control plane
// restart, or left stopped by a failed TunStart) still goes through
// EnsureTun, so its create hook re-runs and it gets started.
func TestReconcileEnsuresAdoptedTun(t *testing.T) {
	n := newTestNode(t)
	h := n.router.host.(*fakeHost)
	h.tuns[5] = true
	n.router.SetRoutes(map[uint32]uint32{5: 5})
	n.router.reconcileOnce()
	for _, op := range h.ops {
		if op == "tun+5" {
			return
		}
	}
	t.Fatalf("EnsureTun(5) never called for an existing tun; ops=%v", h.ops)
}

// Routes are only learned from a link whose liveness session is at least Init.
func TestPathVectorIgnoresDownLink(t *testing.T) {
	n := newTestNode(t)
	l := newLink(n, 7, [32]byte{7}, "", 0)
	n.links[7] = l
	pkt := encodePVPacket([]pvAnnouncement{{dest: 9, path: []uint32{9, 7}}}, nil)

	n.pathVector.handlePacket(l, pkt) // monitor Down (not even started)
	if nh, ok := n.router.LookupRoute(9); ok {
		t.Fatalf("route to 9 via %d learned from a Down link", nh)
	}

	l.monitor.Start()
	defer l.monitor.Stop()
	n.handlePunt(livenessPunt(7, linkStateDown, 0)) // -> Init
	n.pathVector.handlePacket(l, pkt)
	if _, ok := n.router.LookupRoute(9); !ok {
		t.Fatal("route from an Init link (peer went Up first) was dropped")
	}
}

func livenessFrom(link uint32, st linkState, my, your uint32) dpproto.Punt {
	p := livenessPunt(link, st, 0)
	pkt := livenessPacket{State: st, DetectMult: defaultDetectMult, MyDiscriminator: my, YourDiscriminator: your,
		DesiredMinTX: defaultDesiredMinTX, RequiredMinRX: defaultRequiredMinRX}
	p.Payload = pkt.encode()
	return p
}

// A peer still Up with a previous session of ours must not carry it into
// this one: its Up (stale or zero YourDiscriminator) is ignored while we're
// Down, and the handshake completes only via Down -> Init -> Up.
func TestLivenessChecksDiscriminators(t *testing.T) {
	n := newTestNode(t)
	l := newLink(n, 2, [32]byte{2}, "", 0)
	n.links[2] = l
	l.monitor.Start()
	defer l.monitor.Stop()
	mine := l.monitor.localDiscriminator

	n.handlePunt(livenessFrom(2, linkStateUp, 0x55, mine+1)) // addressed to an old session
	n.handlePunt(livenessFrom(2, linkStateUp, 0x55, 0))      // Up without knowing us
	if s := l.monitor.State(); s != linkStateDown {
		t.Fatalf("state = %v after stale Up packets, want Down", s)
	}
	n.handlePunt(livenessFrom(2, linkStateUp, 0x55, mine)) // Up while we're Down: ignored
	if s := l.monitor.State(); s != linkStateDown {
		t.Fatalf("state = %v after Up while Down, want Down", s)
	}
	n.handlePunt(livenessFrom(2, linkStateDown, 0x55, 0))
	if s := l.monitor.State(); s != linkStateInit {
		t.Fatalf("state = %v, want Init", s)
	}
	n.handlePunt(livenessFrom(2, linkStateInit, 0x55, mine))
	if s := l.monitor.State(); s != linkStateUp {
		t.Fatalf("state = %v, want Up", s)
	}
}

func TestParsePrefixRejectsHostBits(t *testing.T) {
	if _, ok := parsePrefix("10.0.1.5/24"); ok {
		t.Fatal("10.0.1.5/24 accepted")
	}
	if _, ok := parsePrefix("10.0.1.0/24"); !ok {
		t.Fatal("10.0.1.0/24 rejected")
	}
	pkt := encodePVPacket([]pvAnnouncement{{dest: 3, path: []uint32{3}, prefixes: []pvPrefix{{addr: 0x0a000105, len: 24}}}}, nil)
	a, _, ok := decodePVPacket(pkt)
	if !ok || a[0].prefixes[0].addr != 0x0a000100 {
		t.Fatalf("decoded prefix not normalized: %+v", a)
	}
}

// Keys that moved between peerids (here: swapped) converge on attach.
func TestSessionHandlesKeySwap(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dp.sock")
	dp := newFakeDP(1)
	dp.links[2] = dpproto.LinkAdd{PeerID: 2, PubKey: [32]byte{3}}
	dp.links[3] = dpproto.LinkAdd{PeerID: 3, PubKey: [32]byte{2}}
	srv, err := dpproto.NewServer(sock, dp)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Run()

	n := newTestNode(t)
	n.device = dpproto.DeviceSet{LocalID: 1, MTU: 1300, ListenPort: 60198}
	n.links[2] = newLink(n, 2, [32]byte{2}, "", 0)
	n.links[3] = newLink(n, 3, [32]byte{3}, "", 0)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); n.runSessions(sock, stop) }()
	defer func() { close(stop); <-done }()

	waitFor(t, "attach", func() bool { return n.dp() != nil })
	dp.mu.Lock()
	defer dp.mu.Unlock()
	if dp.links[2].PubKey != ([32]byte{2}) || dp.links[3].PubKey != ([32]byte{3}) {
		t.Fatalf("links not converged: %+v", dp.links)
	}
}

// An entry sendTo refuses (here: more prefixes than the wire allows) must
// not be recorded as advertised by a full sync.
func TestFullSyncDoesNotMarkUnsendable(t *testing.T) {
	n := newTestNode(t)
	pv := n.pathVector
	pv.sendHook = func(*Link, []pvAnnouncement, []uint32) {}
	many := make([]pvPrefix, 300)
	for i := range many {
		many[i] = pvPrefix{addr: uint32(i) << 8, len: 24}
	}
	pv.best[5] = pvRoute{path: []uint32{5, 3}, prefixes: many}
	pv.best[6] = pvRoute{path: []uint32{6, 3}}
	pv.fullSyncTo(&Link{id: 4})
	if pv.advertised[4][5] {
		t.Fatal("unsendable dest 5 recorded as advertised")
	}
	if !pv.advertised[4][6] || !pv.advertised[4][1] {
		t.Fatalf("sendable dests missing from advertised: %v", pv.advertised[4])
	}
}

// Only one introspection query reaches the data plane at a time.
func TestIntrospectDPAdmitsOne(t *testing.T) {
	n := newTestNode(t)
	n.dpc.Store(&dpHandle{stubDP{}})
	cl, release := n.introspectDP()
	if cl == nil {
		t.Fatal("first query refused")
	}
	if cl2, _ := n.introspectDP(); cl2 != nil {
		t.Fatal("second concurrent query admitted")
	}
	release()
	if cl3, release3 := n.introspectDP(); cl3 == nil {
		t.Fatal("query refused after release")
	} else {
		release3()
	}
}

// stubDP satisfies dpproto.Datapath for tests that never call it.
type stubDP struct{ dpproto.Datapath }
