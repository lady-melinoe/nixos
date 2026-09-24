package main

import (
	"github.com/vishvananda/netlink"
)

// routeProtocolMelnode tags every kernel route melnode installs
// (`ip route show proto 198`), so they're easy to tell apart from anything
// else, and so removal only ever touches routes we installed.
const routeProtocolMelnode netlink.RouteProtocol = 198

// Programming the kernel's routing table for advertised prefixes (so
// traffic for a prefix goes out the right tun) is done by the reconciler,
// see reconcile.go -- no policy routing, no IPIP tunnels: every prefix's
// traffic already rides inside the right peerid's existing Noise-encrypted
// tun, same as everything else melnode routes.

// tunNameForPeerID is the RLock'd read the reconciler uses to turn
// a peerid into the actual OS interface name netlink needs.
func (r *Router) tunNameForPeerID(peerID uint32) (string, bool) {
	r.mu.RLock()
	t, ok := r.tunByPeerID[peerID]
	r.mu.RUnlock()
	if !ok {
		return "", false
	}
	name, err := t.Name()
	if err != nil {
		return "", false
	}
	return name, true
}
