package main

import (
	"github.com/vishvananda/netlink"
)

// kernelroutes.go is the piece that actually replaces melinoe-route's
// own netlink work (see PROJECT_STATE.md): once path-vector
// (pathvector.go) has resolved which peerid currently owns a given
// advertised prefix, something has to program this host's own kernel
// routing table so traffic for it actually goes out the right tun.
// That's all this file does -- no policy routing, no IPIP tunnels
// (melinoe-route used both; melnode doesn't need them since every
// prefix's traffic already rides inside the right peerid's existing
// Noise-encrypted tun, same as everything else melnode routes).

// InstallPrefixRoute programs (or repoints) a kernel route for an
// externally-advertised prefix so this host forwards traffic for it
// onto the right tun. Called by PathVector.applyPrefixChanges whenever
// prefix ownership changes.
//
// A no-op when viaPeerID is this node's own localID: if we're the
// prefix's own origin, there's no tun to route through -- whatever
// locally attached this prefix (a container, a VM) already has its own
// kernel routing sorted out; melnode's job here is only to get *other*
// nodes routing toward us for it, which happens on their end once they
// resolve the winning owner to a tun pointed at us.
func (r *Router) InstallPrefixRoute(prefix pvPrefix, viaPeerID uint32) {
	if viaPeerID == r.localID {
		return
	}
	name, ok := r.tunNameForPeerID(viaPeerID)
	if !ok {
		r.dev.log.Errorf("router: no tun for peerid %d -- can't install kernel route for %v", viaPeerID, prefix)
		return
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		r.dev.log.Errorf("router: netlink lookup for tun %q failed: %v", name, err)
		return
	}
	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       prefix.ipNet(),
	}
	if err := netlink.RouteReplace(route); err != nil {
		r.dev.log.Errorf("router: failed to install kernel route %v via peerid %d (dev %s): %v", prefix, viaPeerID, name, err)
		return
	}
	r.dev.log.Verbosef("router: kernel route %v -> peerid %d (dev %s)", prefix, viaPeerID, name)
}

// RemovePrefixRoute undoes InstallPrefixRoute -- called when a prefix
// becomes unowned (its last claimant is no longer reachable, or
// withdrew).
func (r *Router) RemovePrefixRoute(prefix pvPrefix) {
	route := &netlink.Route{Dst: prefix.ipNet()}
	if err := netlink.RouteDel(route); err != nil {
		// Not upgraded to Errorf: this fires normally whenever the
		// route was never actually installed in the first place (e.g.
		// its owner was always localID, so InstallPrefixRoute no-op'd)
		// -- RouteDel has no "didn't exist" distinction worth surfacing
		// as a real error here.
		r.dev.log.Verbosef("router: removing kernel route for %v: %v (may never have been installed)", prefix, err)
	} else {
		r.dev.log.Verbosef("router: kernel route for %v removed (no longer owned)", prefix)
	}
}

// tunNameForPeerID is the RLock'd read InstallPrefixRoute uses to turn
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
