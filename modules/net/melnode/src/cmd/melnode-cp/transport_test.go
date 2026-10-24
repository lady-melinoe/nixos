package main

import (
	"testing"
	"time"
)

func (n *pvNet) dropData(from, to uint32) int {
	kept, dropped := n.queue[:0], 0
	for _, m := range n.queue {
		if h, _, _, ok := decodePVPacket(m.wire); ok && m.from == from && m.to == to && h.seq != 0 {
			dropped++
			continue
		}
		kept = append(kept, m)
	}
	n.queue = kept
	return dropped
}

func TestLostWithdrawIsRetransmitted(t *testing.T) {
	n := newPVNet(t, 1, 2, 3)
	n.linkUp(1, 2)
	n.linkUp(2, 3)
	if _, ok := n.nextHop(3, 1); !ok {
		t.Fatal("3 should reach 1")
	}

	delete(n.up, linkKey(1, 2))
	n.nodes[1].OnLinkDown(n.peer(1, 2))
	n.nodes[2].OnLinkDown(n.peer(2, 1))
	if n.dropData(2, 3) == 0 {
		t.Fatal("test setup: expected 2 to send 3 a withdraw")
	}
	n.drain()
	if _, ok := n.nextHop(3, 1); !ok {
		t.Fatal("test setup: withdraw should have been lost")
	}

	n.retransmitAll(time.Now().Add(time.Minute))
	if nh, ok := n.nextHop(3, 1); ok {
		t.Fatalf("3 still routes to unreachable 1 via %d after retransmission", nh)
	}
}

func TestReorderedUpdatesApplyInOrder(t *testing.T) {
	n := newPVNet(t, 2, 3)
	n.linkUp(2, 3)
	p1, p2 := mustPrefix(t, "10.1.0.1/32"), mustPrefix(t, "10.1.0.2/32")
	n.nodes[2].AdvertisePrefix(p1)
	n.nodes[2].WithdrawPrefix(p1)
	n.nodes[2].AdvertisePrefix(p2)
	if len(n.queue) < 3 {
		t.Fatalf("expected 3 queued updates, got %d", len(n.queue))
	}
	for i, j := 0, len(n.queue)-1; i < j; i, j = i+1, j-1 {
		n.queue[i], n.queue[j] = n.queue[j], n.queue[i]
	}
	n.drain()
	for i := 0; i < 5; i++ {
		n.retransmitAll(time.Now().Add(time.Duration(i+1) * time.Minute))
	}

	claims := n.nodes[3].prefixClaims
	if _, ok := claims[p1]; ok {
		t.Fatalf("3 still has withdrawn prefix %v: a stale update was applied out of order", p1)
	}
	if _, ok := claims[p2]; !ok {
		t.Fatalf("3 never learned %v", p2)
	}
	if s := n.nodes[2].sessions[3]; len(s.unacked) != 0 {
		t.Fatalf("2 still has %d unacked updates to 3", len(s.unacked))
	}
}

func TestUnacknowledgedSessionResetsLink(t *testing.T) {
	n := newPVNet(t, 1, 2)
	pv := n.nodes[1]
	var failed []uint32
	pv.failLink = func(l *Link) { failed = append(failed, l.id) }
	pv.OnLinkUp(n.peer(1, 2))
	n.queue = nil

	at := time.Now()
	for i := 0; i <= pvMaxRetransmits+2; i++ {
		at = at.Add(time.Minute)
		pv.retransmitDue(at)
		n.queue = nil
	}
	if len(failed) != 1 || failed[0] != 2 {
		t.Fatalf("failLink calls = %v, want exactly [2] after %d unanswered retransmits", failed, pvMaxRetransmits)
	}
}

func TestStaleSessionPacketsRejected(t *testing.T) {
	n := newPVNet(t, 1, 2)
	n.linkUp(1, 2)
	pv := n.nodes[1]
	from := n.peer(1, 2)
	cur := pv.sessions[2]

	stale := encodePVPacket(pvHeader{sendSess: cur.remote, recvSess: cur.local + 1, seq: cur.expect},
		[]pvAnnouncement{{dest: 9, path: []uint32{9, 2}}}, nil)
	pv.handlePacket(from, stale)
	other := encodePVPacket(pvHeader{sendSess: cur.remote + 1, recvSess: cur.local, seq: cur.expect},
		[]pvAnnouncement{{dest: 9, path: []uint32{9, 2}}}, nil)
	pv.handlePacket(from, other)
	if _, ok := pv.best[9]; ok {
		t.Fatal("accepted an update from a stale or foreign session")
	}

	good := encodePVPacket(pvHeader{sendSess: cur.remote, recvSess: cur.local, seq: cur.expect},
		[]pvAnnouncement{{dest: 9, path: []uint32{9, 2}}}, nil)
	pv.handlePacket(from, good)
	if _, ok := pv.best[9]; !ok {
		t.Fatal("rejected an in-session update")
	}
}

func TestWindowLimitsUnacked(t *testing.T) {
	n := newPVNet(t, 1, 2)
	pv := n.nodes[1]
	pv.OnLinkUp(n.peer(1, 2))
	for i := 0; i < 2*pvWindow; i++ {
		pv.AdvertisePrefix(pvPrefix{addr: uint32(0x0a000000 + i), len: 32})
	}
	if got := len(pv.sessions[2].unacked); got != pvWindow {
		t.Fatalf("%d unacked segments, want the window (%d)", got, pvWindow)
	}
	if !pv.dirty[2][1] {
		t.Fatal("expected the latest self-route update to be held back by the window")
	}

	n.up[linkKey(1, 2)] = true
	n.nodes[2].OnLinkUp(n.peer(2, 1))
	n.retransmitAll(time.Now().Add(time.Minute))
	got := n.nodes[2].best[1].prefixes
	if len(got) != 2*pvWindow {
		t.Fatalf("2 learned %d of node 1's prefixes after the window drained, want %d", len(got), 2*pvWindow)
	}
}
