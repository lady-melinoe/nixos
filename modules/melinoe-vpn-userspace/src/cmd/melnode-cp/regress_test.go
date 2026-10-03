package main

import (
	"testing"

	"melnode/dpproto"
)

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

func TestPathVectorIgnoresDownLink(t *testing.T) {
	n := newTestNode(t)
	l := newLink(n, 7, [32]byte{7}, "", 0)
	n.links[7] = l
	pkt := encodePVPacket([]pvAnnouncement{{dest: 9, path: []uint32{9, 7}}}, nil)

	n.pathVector.handlePacket(l, pkt)
	if nh, ok := n.router.LookupRoute(9); ok {
		t.Fatalf("route to 9 via %d learned from a Down link", nh)
	}

	l.monitor.Start()
	defer l.monitor.Stop()
	n.handlePunt(livenessPunt(7, linkStateDown, 0))
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

func TestLivenessChecksDiscriminators(t *testing.T) {
	n := newTestNode(t)
	l := newLink(n, 2, [32]byte{2}, "", 0)
	n.links[2] = l
	l.monitor.Start()
	defer l.monitor.Stop()
	mine := l.monitor.localDiscriminator

	n.handlePunt(livenessFrom(2, linkStateUp, 0x55, mine+1))
	n.handlePunt(livenessFrom(2, linkStateUp, 0x55, 0))
	if s := l.monitor.State(); s != linkStateDown {
		t.Fatalf("state = %v after stale Up packets, want Down", s)
	}
	n.handlePunt(livenessFrom(2, linkStateUp, 0x55, mine))
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

type stubDP struct{ dpproto.Datapath }
