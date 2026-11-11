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
	t.Fatalf("EnsurePeer(5) never called for an existing tun; ops=%v", h.ops)
}

func TestPathVectorNeedsSession(t *testing.T) {
	n := newTestNode(t)
	l := newLink(n, 7, [32]byte{7}, "", 0)
	n.links[7] = l
	pkt := encodePVPacket(pvHeader{sendSess: 0x77, seq: 1}, []pvAnnouncement{{dest: 9, path: []uint32{9, 7}}}, nil)

	n.pathVector.handlePacket(l, pkt)
	if nh, ok := n.router.LookupRoute(9); ok {
		t.Fatalf("route to 9 via %d learned before the link came up", nh)
	}

	n.pathVector.OnLinkUp(l)
	n.pathVector.handlePacket(l, pkt)
	if _, ok := n.router.LookupRoute(9); !ok {
		t.Fatal("in-session update from an Up link was dropped")
	}
}

func TestLivenessEventsReachStartedMonitorOnly(t *testing.T) {
	n := newTestNode(t)
	l := newLink(n, 2, [32]byte{2}, "", 0)
	n.links[2] = l
	l.monitor.onEvent(dpproto.LivenessEvent{Link: 2, State: uint8(linkStateUp)})
	if s := l.monitor.State(); s != linkStateDown {
		t.Fatalf("state = %v after an event before Start, want Down", s)
	}
	var got []linkState
	l.monitor.SetOnStateChange(func(_ *Link, s linkState) { got = append(got, s) })
	l.monitor.Start()
	l.monitor.onEvent(dpproto.LivenessEvent{Link: 2, State: uint8(linkStateInit)})
	l.monitor.onEvent(dpproto.LivenessEvent{Link: 2, State: uint8(linkStateUp)})
	l.monitor.onEvent(dpproto.LivenessEvent{Link: 2, State: uint8(linkStateUp)})
	if s := l.monitor.State(); s != linkStateUp {
		t.Fatalf("state = %v, want Up", s)
	}
	if len(got) != 2 || got[0] != linkStateInit || got[1] != linkStateUp {
		t.Fatalf("callbacks = %v, want [Init Up]", got)
	}
	l.monitor.Stop()
	l.monitor.onEvent(dpproto.LivenessEvent{Link: 2, State: uint8(linkStateDown)})
	if s := l.monitor.State(); s != linkStateUp {
		t.Fatalf("state = %v after an event once stopped, want Up", s)
	}
}

func TestParsePrefixRejectsHostBits(t *testing.T) {
	if _, ok := parsePrefix("10.0.1.5/24"); ok {
		t.Fatal("10.0.1.5/24 accepted")
	}
	if _, ok := parsePrefix("10.0.1.0/24"); !ok {
		t.Fatal("10.0.1.0/24 rejected")
	}
	pkt := encodePVPacket(pvHeader{sendSess: 1}, []pvAnnouncement{{dest: 3, path: []uint32{3}, prefixes: []pvPrefix{{addr: 0x0a000105, len: 24}}}}, nil)
	_, a, _, ok := decodePVPacket(pkt)
	if !ok || a[0].prefixes[0].addr != 0x0a000100 {
		t.Fatalf("decoded prefix not normalized: %+v", a)
	}
}

type stubDP struct{ dpproto.Datapath }
