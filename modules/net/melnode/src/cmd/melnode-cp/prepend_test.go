package main

import "testing"

func TestOriginPrependCounts(t *testing.T) {
	n := newPVNet(t, 1, 5, 8, 10)
	n.peer(10, 1).prependCount = 1
	n.peer(10, 5).prependCount = 0
	n.peer(1, 8).prependCount = 4
	n.peer(5, 8).prependCount = 4
	n.linkUp(10, 1)
	n.linkUp(10, 5)
	n.linkUp(1, 8)
	n.linkUp(5, 8)
	n.settle()

	nh, ok := n.nextHop(8, 10)
	if !ok || nh != 5 {
		t.Fatalf("8 -> 10 next hop = %d (ok=%v), want 5: origin prepend on 10->1 was not charged", nh, ok)
	}
	if got := n.nodes[1].best[10].path; len(got) != 2 {
		t.Fatalf("1's path to 10 = %v, want 10 announced twice (prepend 1)", got)
	}
}
