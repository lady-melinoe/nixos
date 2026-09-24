package main

import (
	"github.com/vishvananda/netlink"
)

// routeProtocolMelnode tags every kernel route melnode installs
// (`ip route show proto 198`), so they're easy to tell apart from anything
// else, and so removal only ever touches routes we installed.
const routeProtocolMelnode netlink.RouteProtocol = 198

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
// When viaPeerID is this node's own localID there's no tun to route
// through: whatever locally attached this prefix (a container, a VM)
// already has its own kernel routing sorted out, and melnode's job is only
// to get *other* nodes routing toward us for it, which happens on their end
// once they resolve the winning owner to a tun pointed at us. The one thing
// to do here is drop a stale route left over from a previous remote owner
// (see below).
func (r *Router) InstallPrefixRoute(prefix pvPrefix, viaPeerID uint32) {
	if viaPeerID == r.localID {
		// The prefix may only just have become ours (we started advertising
		// it, or won the path-length tiebreak) after a remote node owned it,
		// in which case we still hold the route installed for that previous
		// owner. Left in place it keeps forwarding traffic out the old tun
		// instead of delivering it locally. Only routes tagged
		// routeProtocolMelnode match, so whatever locally attached the
		// prefix (a container/VM route) is untouched; "no such process" is
		// the normal case (nothing was installed) and not worth reporting.
		stale := &netlink.Route{Dst: prefix.ipNet(), Protocol: routeProtocolMelnode}
		if err := netlink.RouteDel(stale); err == nil {
			r.dev.log.Verbosef("router: removed stale kernel route for %v (now locally owned)", prefix)
		}
		return
	}
	// If this peer's tun is still being created (another goroutine's
	// AddOrUpdateRoute), wait for that to finish rather than failing.
	pl := r.peerLock(viaPeerID)
	pl.Lock()
	name, ok := r.tunNameForPeerID(viaPeerID)
	pl.Unlock()
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
		Protocol:  routeProtocolMelnode,
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
	route := &netlink.Route{Dst: prefix.ipNet(), Protocol: routeProtocolMelnode}
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
