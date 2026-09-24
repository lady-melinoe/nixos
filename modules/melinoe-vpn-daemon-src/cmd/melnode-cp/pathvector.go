package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// pathvector.go implements the path-vector (BGP-alike) routing protocol
// mentioned in PROJECT_STATE.md: it replaces static [[peer]]/nhid config
// with dst peerid -> nhid routes discovered by exchanging reachability
// advertisements with each currently-live neighbor (a Link, i.e. a
// [[link]]). "Live" is entirely linkmonitor.go's call -- nothing is
// advertised over, or trusted from, a link whose LinkMonitor isn't Up,
// and a link going Down withdraws immediately (per earlier discussion)
// rather than waiting for advertisements to time out.
//
// Destinations are node peerids, same as always -- but a node can now
// also claim ownership of arbitrary IPv4 prefixes (pvPrefix) alongside
// its own peerid self-origination, per PROJECT_STATE.md's route-
// advertisement work: an external process (a container/VM scheduler,
// not melnode itself) calls PathVector.AdvertisePrefix/WithdrawPrefix
// via the local control API (controlapi.go) to say "I'm now
// responsible for this prefix", and it rides along inside this node's
// own reachability announcement, using the exact same propagation,
// loop-prevention, and withdrawal machinery as peerid routes already
// do -- no separate gossip mechanism.
//
// When more than one node claims the same prefix (e.g. a container
// migrated but the old advertisement hasn't been withdrawn yet), the
// winner is picked by reusing path-vector's own shortest-path
// selection (recomputeBestLocked's rule, tie-broken by lowest origin
// peerid) rather than inventing a separate priority scheme -- see
// recomputePrefixOwnerLocked.
//
// Wire format rides proto=2, Vers=1, sibling to proto=0 (tunneled IP)
// and proto=1 (liveness, linkmonitor.go). Unlike proto=1's fixed-size
// payload, a route update is naturally variable-length (however many
// destinations/prefixes are being announced/withdrawn this round), so
// here the Vers=1 payload's own Length field does real self-description
// work, not just symbolic parity with proto=0/1's convention.
//
// Loop prevention is path-vector's namesake mechanism (BGP's AS_PATH,
// scaled down to melnode peerids): every announcement carries the full
// chain of node-ids the route has passed through since it was
// originated, and a node drops (or refuses to further propagate) any
// announcement whose path already contains its own id. This also gives
// split-horizon almost for free: a route is never re-advertised back
// toward a neighbor whose id already appears in its path, since that
// neighbor already knows about it and re-sending would just be
// wasted/looping chatter (not a correctness issue on its own -- the
// receiving end would reject it via the same loop check -- but there's
// no reason to send it).
//
// AS-prepending ([[link]] prependCount, see forwardPath) is path-
// vector's traffic-engineering lever: a node relaying routes out over
// a specific link can inflate the path length anyone downstream sees
// for anything relayed that way, without a separate cost metric --
// useful when plain hop count would otherwise prefer a low-hop
// long-haul link over a cheaper multi-hop in-DC path.

const (
	pvProto = 2 // sibling to proto=0 (tunneled IP), proto=1 (liveness)
	pvVers1 = 1

	// proto=2, Vers=1 payload layout:
	//   0:    Vers          (uint8)
	//   1:    reserved      (uint8, zero)
	//   2:4:  Length        (uint16, big-endian, self-declared -- see
	//         punt.go's proto=2 dispatch)
	//   4:6:  NumAnnounce   (uint16, big-endian)
	//   6:8:  NumWithdraw   (uint16, big-endian)
	//   then NumAnnounce entries, each:
	//     Dest      (uint8)
	//     PathLen   (uint8)
	//     Path      (PathLen bytes, one per hop, origin first)
	//     NumPrefix (uint8)
	//     Prefixes  (NumPrefix * 5 bytes: 1 length byte + 4-byte IPv4
	//                address, big-endian, per prefix -- what Dest
	//                itself currently claims to own, unchanged by
	//                relaying, see forwardPath's doc comment)
	//   then NumWithdraw entries, each:
	//     Dest    (uint8)
	//
	// This is an in-place extension of Vers=1 (prefixes weren't there
	// originally) rather than a new Vers, since nothing else has ever
	// spoken this wire format -- see PROJECT_STATE.md.
	pvHeaderSize = 8

	// How often a full table (all current best routes, split-horizoned
	// per recipient, plus our own self-origin entry) is sent to every
	// currently-Up neighbor -- belt-and-suspenders against a missed
	// triggered update (e.g. to a lost packet -- proto=2 isn't
	// acked/retransmitted), on top of send-immediately-on-change.
	pvFullSyncInterval = 30 * time.Second

	// Mesh diameter bound: a path this long can only be a loop or
	// garbage, given peerids are 0-255 and (barring a very unusual
	// topology) no real path should ever approach this. Cheap sanity
	// check, not a real protocol limit.
	pvMaxPathLen = 255
)

// pvPrefix is an IPv4 prefix, wire-compact (5 bytes: 1 length + 4
// address). IPv6 isn't supported -- matches the rest of melnode's
// IPv4-only scope so far (see PROJECT_STATE.md).
type pvPrefix struct {
	addr uint32 // big-endian numeric value of the 4 address octets
	len  uint8  // 0-32
}

func (p pvPrefix) String() string {
	return fmt.Sprintf("%d.%d.%d.%d/%d", byte(p.addr>>24), byte(p.addr>>16), byte(p.addr>>8), byte(p.addr), p.len)
}

// parsePrefix accepts either CIDR ("10.0.1.3/32") or a bare address
// ("10.0.1.3", treated as /32 for convenience -- the common case for a
// single container/VM address).
func parsePrefix(s string) (pvPrefix, bool) {
	if !strings.Contains(s, "/") {
		s += "/32"
	}
	ip, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		return pvPrefix{}, false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return pvPrefix{}, false
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return pvPrefix{}, false
	}
	return pvPrefix{addr: binary.BigEndian.Uint32(ip4), len: uint8(ones)}, true
}

func (p pvPrefix) ipNet() *net.IPNet {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, p.addr)
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(int(p.len), 32)}
}

func prefixesEqual(a, b []pvPrefix) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[pvPrefix]bool, len(a))
	for _, p := range a {
		seen[p] = true
	}
	for _, p := range b {
		if !seen[p] {
			return false
		}
	}
	return true
}

// pvRoute is one candidate route to a destination, as advertised by one
// neighbor.
type pvRoute struct {
	path     []uint32   // origin (the dest itself) ... the neighbor that told us, inclusive
	prefixes []pvPrefix // whatever the origin (path[0]) itself currently claims to own
}

func (r pvRoute) viaNeighbor() uint32 {
	return r.path[len(r.path)-1]
}

func (r pvRoute) contains(id uint32) bool {
	for _, p := range r.path {
		if p == id {
			return true
		}
	}
	return false
}

// prefixOwnerChange is the result of recomputePrefixOwnerLocked/
// reconcilePrefixesLocked's comparison against the previously-applied
// owner -- ok=false means the prefix is now unowned (its last claimant
// became unreachable, or withdrew) and any installed kernel route
// should be removed.
type prefixOwnerChange struct {
	prefix pvPrefix
	owner  uint32
	ok     bool
}

// PathVector owns route discovery for the whole mesh as seen from this
// node: one PathVector per Node, talking proto=2 with every currently
// Up-per-linkmonitor Link.
type PathVector struct {
	node    *Node
	localID uint32

	// tblMu serializes "snapshot pv.best, push it to the router" so the
	// newest snapshot is always the last one written (see syncKernel).
	// Lock order: tblMu, then mu, then the router's locks. Only ever held
	// for in-memory work.
	tblMu sync.Mutex

	mu sync.Mutex
	// learned[dest][neighborPeerID] = that neighbor's currently
	// advertised route to dest. Kept per-neighbor (not just the winner)
	// so that losing one neighbor's route (withdrawal, or the link
	// itself going down) lets us fall back to another without having
	// thrown the alternative away.
	learned map[uint32]map[uint32]pvRoute
	// best[dest] = the currently-installed route (mirrors
	// node.router.routeTable, but path-vector needs the full path, not
	// just the nhid, to do loop prevention/split-horizon on the way
	// back out).
	best map[uint32]pvRoute

	neighbors map[uint32]*Link // currently-Up links we're running the protocol with

	// advertised[neighborID][dest] = we've told that neighbor about a route
	// to dest and not yet withdrawn it. This is what lets split-horizon
	// (and the too-long-path guard) *withdraw* a previously-advertised
	// route when it stops being advertisable to that neighbor, instead of
	// silently skipping it and leaving a stale learned[dest][us] entry in
	// the neighbor's table -- see suppressLocked / propagateAnnounce.
	// Cleared for a neighbor whenever its link goes down or comes up.
	advertised map[uint32]map[uint32]bool

	// localPrefixes is this node's own claimed set: IdentityPrefix
	// (config, always present if set) plus whatever's been added via
	// AdvertisePrefix/WithdrawPrefix (controlapi.go). Rides inside this
	// node's own self-origination (dest=localID) in every announcement,
	// exactly like path[0] does.
	localPrefixes map[pvPrefix]bool

	// prefixClaims[p] = set of peerids currently claiming to own prefix
	// p, per the *winning* route for each dest (see the announce/
	// withdraw loops in handlePacket -- claims are updated on a dest's
	// best route changing, not on every raw learned update).
	prefixClaims map[pvPrefix]map[uint32]bool
	// prefixOwner[p] = the peerid this node last resolved as p's owner
	// (the desired prefix -> owner mapping the reconciler installs
	// kernel routes from, see reconcile.go).
	prefixOwner map[pvPrefix]uint32

	stopCh chan struct{}
	wg     sync.WaitGroup

	// sendHook, if non-nil, replaces the real wire send in sendTo -- test
	// seam only (pathvector_test.go wires several PathVectors together
	// in-memory); always nil in production.
	sendHook func(peer *Link, announces []pvAnnouncement, withdraws []uint32)
}

func newPathVector(node *Node, localID uint32) *PathVector {
	pv := &PathVector{
		node:          node,
		localID:       localID,
		learned:       make(map[uint32]map[uint32]pvRoute),
		best:          make(map[uint32]pvRoute),
		neighbors:     make(map[uint32]*Link),
		advertised:    make(map[uint32]map[uint32]bool),
		localPrefixes: make(map[pvPrefix]bool),
		prefixClaims:  make(map[pvPrefix]map[uint32]bool),
		prefixOwner:   make(map[pvPrefix]uint32),
	}
	if node.router != nil {
		node.router.desiredPrefixes = pv.snapshotPrefixOwners
	}
	return pv
}

// Start begins the periodic full-resync loop. Per-neighbor sessions
// themselves are driven by OnLinkUp/OnLinkDown (wired to each Link's
// LinkMonitor in main.go), not by anything started here.
func (pv *PathVector) Start() {
	pv.stopCh = make(chan struct{})
	pv.node.router.StartReconciler(pv.stopCh, &pv.wg)
	pv.wg.Add(1)
	go pv.fullSyncLoop()
}

func (pv *PathVector) Stop() {
	close(pv.stopCh)
	pv.wg.Wait()
}

func (pv *PathVector) fullSyncLoop() {
	defer pv.wg.Done()
	ticker := time.NewTicker(pvFullSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-pv.stopCh:
			return
		case <-ticker.C:
			pv.mu.Lock()
			neighbors := pv.neighborsSnapshotLocked()
			pv.mu.Unlock()
			for _, p := range neighbors {
				pv.fullSyncTo(p)
			}
			pv.syncKernel() // safety net: heal drift the event paths missed
		}
	}
}

// syncKernel pushes path-vector's current view to the router and kicks
// the reconciler (reconcile.go), which does the actual tun/route work off
// this goroutine. The dest -> next-hop table is rebuilt from a fresh
// snapshot of pv.best each call, under tblMu, so however callers interleave
// the last write always reflects the newest state; nothing here does I/O.
func (pv *PathVector) syncKernel() {
	pv.tblMu.Lock()
	defer pv.tblMu.Unlock()
	pv.mu.Lock()
	routes := make(map[uint32]uint32, len(pv.best))
	for dest, route := range pv.best {
		routes[dest] = route.viaNeighbor()
	}
	pv.mu.Unlock()
	pv.node.router.SetRoutes(routes)
}

// snapshotPrefixOwners is the reconciler's source for which peerid owns
// each advertised prefix right now.
func (pv *PathVector) snapshotPrefixOwners() map[pvPrefix]uint32 {
	pv.mu.Lock()
	defer pv.mu.Unlock()
	out := make(map[pvPrefix]uint32, len(pv.prefixOwner))
	for p, o := range pv.prefixOwner {
		out[p] = o
	}
	return out
}

// OnLinkUp is wired as the onStateChange callback (see linkmonitor.go)
// for the Up transition: brings this neighbor into the protocol and
// gives it a full initial table dump, mirroring BGP's own
// session-established behavior.
func (pv *PathVector) OnLinkUp(peer *Link) {
	pv.mu.Lock()
	pv.neighbors[peer.id] = peer
	delete(pv.advertised, peer.id) // fresh session: the neighbor knows nothing of ours
	pv.mu.Unlock()
	pv.node.log.Verbosef("pathvector: neighbor %v up, sending full table", peer)
	pv.fullSyncTo(peer)
}

// OnLinkDown withdraws immediately (per earlier discussion): forgets
// every route this neighbor had told us, recomputes best for anything
// affected, and propagates the fallout (new-best announcements or
// withdrawals) to remaining neighbors right away rather than waiting
// for a timeout. Also reconciles any prefixes that were claimed
// through a dest that just became unreachable.
func (pv *PathVector) OnLinkDown(peer *Link) {
	pv.mu.Lock()
	delete(pv.neighbors, peer.id)
	delete(pv.advertised, peer.id) // it drops everything we told it on its own link-down

	type change struct {
		dest       uint32
		newBest    pvRoute
		reachable  bool
		wasChanged bool
	}
	var changes []change
	affectedPrefixes := make(map[pvPrefix]bool)

	for dest, byNeighbor := range pv.learned {
		if _, hadIt := byNeighbor[peer.id]; !hadIt {
			continue
		}
		delete(byNeighbor, peer.id)
		if len(byNeighbor) == 0 {
			delete(pv.learned, dest)
		}
		newBest, ok := pv.recomputeBestLocked(dest)
		old, hadOld := pv.best[dest]
		if !ok {
			delete(pv.best, dest)
			changes = append(changes, change{dest: dest, reachable: false, wasChanged: hadOld})
			if hadOld {
				for p := range pv.updatePrefixClaimsLocked(dest, old.prefixes, nil) {
					affectedPrefixes[p] = true
				}
			}
		} else if !hadOld || !pathsEqual(old.path, newBest.path) || !prefixesEqual(old.prefixes, newBest.prefixes) {
			pv.best[dest] = newBest
			changes = append(changes, change{dest: dest, newBest: newBest, reachable: true, wasChanged: true})
			var oldPrefixes []pvPrefix
			if hadOld {
				oldPrefixes = old.prefixes
			}
			for p := range pv.updatePrefixClaimsLocked(dest, oldPrefixes, newBest.prefixes) {
				affectedPrefixes[p] = true
			}
		}
	}
	prefixChanges := pv.reconcilePrefixesLocked(affectedPrefixes)
	neighbors := pv.neighborsSnapshotLocked()
	pv.mu.Unlock()

	for _, c := range changes {
		if !c.wasChanged {
			continue
		}
		if !c.reachable {
			pv.propagateWithdraw(c.dest, neighbors, nil)
		} else {
			pv.propagateAnnounce(c.dest, c.newBest, neighbors, nil)
		}
	}
	if len(changes) > 0 || len(prefixChanges) > 0 {
		pv.syncKernel()
	}
}

// handlePacket is called from punt.go's proto=2 dispatch with the
// already Length-trimmed payload.
func (pv *PathVector) handlePacket(from *Link, payload []byte) {
	announces, withdraws, ok := decodePVPacket(payload)
	if !ok {
		pv.node.log.Verbosef("%v - pathvector: malformed packet, dropping", from)
		return
	}

	pv.mu.Lock()
	var announceChanges []struct {
		dest  uint32
		route pvRoute
	}
	var withdrawChanges []struct {
		dest      uint32
		newBest   pvRoute
		reachable bool
	}
	affectedPrefixes := make(map[pvPrefix]bool)

	for _, a := range announces {
		if a.dest == pv.localID {
			// Someone is advertising a route to us -- either a stale
			// echo or another node misconfigured with our id. Either
			// way, not something we can sensibly install; drop.
			continue
		}
		if len(a.path) == 0 || len(a.path) > pvMaxPathLen {
			continue
		}
		route := pvRoute{path: a.path, prefixes: a.prefixes}
		if route.contains(pv.localID) {
			// Loop: this path already passed through us.
			continue
		}
		if route.viaNeighbor() != from.id {
			// The path's last hop should always be whoever's sending
			// it to us -- if not, something's wrong (spoofed/corrupt
			// path); drop rather than trust it.
			continue
		}
		if _, ok := pv.learned[a.dest]; !ok {
			pv.learned[a.dest] = make(map[uint32]pvRoute)
		}
		pv.learned[a.dest][from.id] = route

		newBest, _ := pv.recomputeBestLocked(a.dest)
		old, hadOld := pv.best[a.dest]
		if !hadOld || !pathsEqual(old.path, newBest.path) || !prefixesEqual(old.prefixes, newBest.prefixes) {
			pv.best[a.dest] = newBest
			announceChanges = append(announceChanges, struct {
				dest  uint32
				route pvRoute
			}{a.dest, newBest})
			var oldPrefixes []pvPrefix
			if hadOld {
				oldPrefixes = old.prefixes
			}
			for p := range pv.updatePrefixClaimsLocked(a.dest, oldPrefixes, newBest.prefixes) {
				affectedPrefixes[p] = true
			}
		}
	}

	for _, dest := range withdraws {
		byNeighbor, ok := pv.learned[dest]
		if !ok {
			continue
		}
		if _, hadIt := byNeighbor[from.id]; !hadIt {
			continue
		}
		delete(byNeighbor, from.id)
		if len(byNeighbor) == 0 {
			delete(pv.learned, dest)
		}
		newBest, ok := pv.recomputeBestLocked(dest)
		old, hadOld := pv.best[dest]
		if !ok {
			delete(pv.best, dest)
			if hadOld {
				withdrawChanges = append(withdrawChanges, struct {
					dest      uint32
					newBest   pvRoute
					reachable bool
				}{dest, pvRoute{}, false})
				for p := range pv.updatePrefixClaimsLocked(dest, old.prefixes, nil) {
					affectedPrefixes[p] = true
				}
			}
		} else if !hadOld || !pathsEqual(old.path, newBest.path) || !prefixesEqual(old.prefixes, newBest.prefixes) {
			pv.best[dest] = newBest
			withdrawChanges = append(withdrawChanges, struct {
				dest      uint32
				newBest   pvRoute
				reachable bool
			}{dest, newBest, true})
			var oldPrefixes []pvPrefix
			if hadOld {
				oldPrefixes = old.prefixes
			}
			for p := range pv.updatePrefixClaimsLocked(dest, oldPrefixes, newBest.prefixes) {
				affectedPrefixes[p] = true
			}
		}
	}

	prefixChanges := pv.reconcilePrefixesLocked(affectedPrefixes)
	neighbors := pv.neighborsSnapshotLocked()
	pv.mu.Unlock()

	for _, c := range announceChanges {
		pv.propagateAnnounce(c.dest, c.route, neighbors, from)
	}
	for _, c := range withdrawChanges {
		if c.reachable {
			pv.propagateAnnounce(c.dest, c.newBest, neighbors, from)
		} else {
			pv.propagateWithdraw(c.dest, neighbors, from)
		}
	}
	if len(announceChanges) > 0 || len(withdrawChanges) > 0 || len(prefixChanges) > 0 {
		pv.syncKernel()
	}
}

// AdvertisePrefix and WithdrawPrefix are the control API's (controlapi.go)
// entry points -- an external process (a container/VM scheduler, per
// PROJECT_STATE.md) telling melnode what it's now responsible for.
func (pv *PathVector) AdvertisePrefix(p pvPrefix) { pv.setLocalPrefix(p, true) }
func (pv *PathVector) WithdrawPrefix(p pvPrefix)  { pv.setLocalPrefix(p, false) }

func (pv *PathVector) setLocalPrefix(p pvPrefix, advertise bool) {
	pv.mu.Lock()
	if pv.localPrefixes[p] == advertise {
		pv.mu.Unlock()
		return
	}
	oldPrefixes := pv.localPrefixesListLocked()
	if advertise {
		pv.localPrefixes[p] = true
	} else {
		delete(pv.localPrefixes, p)
	}
	newPrefixes := pv.localPrefixesListLocked()
	affected := pv.updatePrefixClaimsLocked(pv.localID, oldPrefixes, newPrefixes)
	prefixChanges := pv.reconcilePrefixesLocked(affected)
	neighbors := pv.neighborsSnapshotLocked()
	route := pvRoute{path: []uint32{pv.localID}, prefixes: newPrefixes}
	pv.mu.Unlock()

	if advertise {
		pv.node.log.Verbosef("pathvector: now advertising %v", p)
	} else {
		pv.node.log.Verbosef("pathvector: no longer advertising %v", p)
	}
	if len(prefixChanges) > 0 {
		pv.syncKernel()
	}
	pv.propagateAnnounce(pv.localID, route, neighbors, nil)
}

func (pv *PathVector) localPrefixesListLocked() []pvPrefix {
	out := make([]pvPrefix, 0, len(pv.localPrefixes))
	for p := range pv.localPrefixes {
		out = append(out, p)
	}
	return out
}

// updatePrefixClaimsLocked reconciles prefixClaims for one dest (which
// may be pv.localID itself, for our own advertisements) whose best
// route just changed, given its previous and new claimed-prefix sets.
// Returns every prefix whose ownership might now need recomputing --
// the union of old+new, since a path-length-only change with no
// prefix-set change can still flip ownership between two claimants of
// the same prefix. Caller holds pv.mu.
func (pv *PathVector) updatePrefixClaimsLocked(dest uint32, oldPrefixes, newPrefixes []pvPrefix) map[pvPrefix]bool {
	affected := make(map[pvPrefix]bool, len(oldPrefixes)+len(newPrefixes))
	newSet := make(map[pvPrefix]bool, len(newPrefixes))
	for _, p := range newPrefixes {
		newSet[p] = true
		affected[p] = true
		if pv.prefixClaims[p] == nil {
			pv.prefixClaims[p] = make(map[uint32]bool)
		}
		pv.prefixClaims[p][dest] = true
	}
	for _, p := range oldPrefixes {
		affected[p] = true
		if newSet[p] {
			continue
		}
		if claimants, ok := pv.prefixClaims[p]; ok {
			delete(claimants, dest)
			if len(claimants) == 0 {
				delete(pv.prefixClaims, p)
			}
		}
	}
	return affected
}

// recomputePrefixOwnerLocked picks, among every peerid currently
// claiming prefix p and still reachable (or pv.localID itself, always
// "reachable" at effectively zero distance since it's local
// origination), the one with the shortest path -- reusing
// recomputeBestLocked's exact rule rather than a separate priority
// scheme, per PROJECT_STATE.md's discussion of why a real path-length
// signal beats an invented one. Ties broken by lowest origin peerid.
// Caller holds pv.mu.
func (pv *PathVector) recomputePrefixOwnerLocked(p pvPrefix) (uint32, bool) {
	claimants, ok := pv.prefixClaims[p]
	if !ok || len(claimants) == 0 {
		return 0, false
	}
	var bestID uint32
	bestLen := 0
	found := false
	for id := range claimants {
		pathLen := 0
		if id != pv.localID {
			route, reachable := pv.best[id]
			if !reachable {
				continue // claimed by a peerid we can no longer reach at all
			}
			pathLen = len(route.path)
		}
		if !found || pathLen < bestLen || (pathLen == bestLen && id < bestID) {
			bestID, bestLen, found = id, pathLen, true
		}
	}
	return bestID, found
}

// reconcilePrefixesLocked recomputes ownership for every prefix in
// affected and updates pv.prefixOwner, returning the ones that actually
// changed (the caller kicks the reconciler if any did, see reconcile.go).
// Caller holds pv.mu.
func (pv *PathVector) reconcilePrefixesLocked(affected map[pvPrefix]bool) []prefixOwnerChange {
	var changes []prefixOwnerChange
	for p := range affected {
		newOwner, ok := pv.recomputePrefixOwnerLocked(p)
		oldOwner, hadOwner := pv.prefixOwner[p]
		if !ok {
			if hadOwner {
				delete(pv.prefixOwner, p)
				changes = append(changes, prefixOwnerChange{prefix: p, ok: false})
			}
			continue
		}
		if !hadOwner || oldOwner != newOwner {
			pv.prefixOwner[p] = newOwner
			changes = append(changes, prefixOwnerChange{prefix: p, owner: newOwner, ok: true})
		}
	}
	return changes
}

// recomputeBestLocked picks the shortest known path to dest across all
// neighbors that have told us one, tie-breaking on lowest neighbor id
// for determinism. Caller holds pv.mu.
func (pv *PathVector) recomputeBestLocked(dest uint32) (pvRoute, bool) {
	byNeighbor, ok := pv.learned[dest]
	if !ok || len(byNeighbor) == 0 {
		return pvRoute{}, false
	}
	var best pvRoute
	found := false
	for nid, route := range byNeighbor {
		if !found || len(route.path) < len(best.path) || (len(route.path) == len(best.path) && nid < best.viaNeighbor()) {
			best = route
			found = true
		}
	}
	return best, found
}

func (pv *PathVector) neighborsSnapshotLocked() []*Link {
	out := make([]*Link, 0, len(pv.neighbors))
	for _, p := range pv.neighbors {
		out = append(out, p)
	}
	return out
}

// propagateAnnounce sends dest's new best route to every neighbor,
// except that a neighbor for whom the route can't (or shouldn't) be
// advertised -- it's skipSource (who just told us about it, pointless to
// echo back), it already appears in the path (split-horizon/loop-
// avoidance, see this file's top comment), or the path would be too long
// to encode -- instead gets a *withdraw* if we'd previously advertised
// dest to it. Silently skipping (the original behavior) left the
// neighbor holding our older, now-wrong route as a stale alternative,
// which it would happily fall back to on its own next failure and
// forward straight back at us.
func (pv *PathVector) propagateAnnounce(dest uint32, route pvRoute, neighbors []*Link, skipSource *Link) {
	for _, n := range neighbors {
		var fwd []uint32
		suppress := (skipSource != nil && n.id == skipSource.id) || route.contains(n.id)
		if !suppress {
			fwd = pv.forwardPath(route.path, n)
			suppress = len(fwd) > pvMaxPathLen
		}
		if suppress {
			if pv.clearAdvertised(n.id, dest) {
				pv.sendTo(n, nil, []uint32{dest})
			}
			continue
		}
		pv.markAdvertised(n.id, dest)
		pv.sendTo(n, []pvAnnouncement{{dest: dest, path: fwd, prefixes: route.prefixes}}, nil)
	}
}

// propagateWithdraw tells every neighbor we'd advertised dest to that it's
// gone. That includes skipSource: if we had advertised dest to the
// neighbor that just withdrew it from us, its copy is stale too.
func (pv *PathVector) propagateWithdraw(dest uint32, neighbors []*Link, skipSource *Link) {
	for _, n := range neighbors {
		if pv.clearAdvertised(n.id, dest) {
			pv.sendTo(n, nil, []uint32{dest})
		}
	}
}

func (pv *PathVector) markAdvertised(nid, dest uint32) {
	pv.mu.Lock()
	defer pv.mu.Unlock()
	m := pv.advertised[nid]
	if m == nil {
		m = make(map[uint32]bool)
		pv.advertised[nid] = m
	}
	m[dest] = true
}

// clearAdvertised reports whether dest had been advertised to nid.
func (pv *PathVector) clearAdvertised(nid, dest uint32) bool {
	pv.mu.Lock()
	defer pv.mu.Unlock()
	m := pv.advertised[nid]
	if !m[dest] {
		return false
	}
	delete(m, dest)
	return true
}

// fullSyncTo sends every currently-selected best route (split-horizoned
// for this recipient) plus our own self-origin entry, in one packet.
// Used both for a brand-new neighbor's initial table dump and for the
// periodic resync. Anything we'd previously advertised to this neighbor
// that isn't in that set any more is withdrawn in the same packet, so
// the periodic resync also heals a lost withdraw (proto=2 isn't
// acked/retransmitted). sendTo chunks it to the MTU.
func (pv *PathVector) fullSyncTo(peer *Link) {
	pv.mu.Lock()
	entries := make([]pvAnnouncement, 0, len(pv.best)+1)
	sent := make(map[uint32]bool, len(pv.best)+1)
	entries = append(entries, pvAnnouncement{dest: pv.localID, path: []uint32{pv.localID}, prefixes: pv.localPrefixesListLocked()})
	sent[pv.localID] = true
	for dest, route := range pv.best {
		if route.contains(peer.id) {
			continue // split horizon (withdrawn below if previously advertised)
		}
		fwd := pv.forwardPath(route.path, peer)
		if len(fwd) > pvMaxPathLen {
			continue
		}
		entries = append(entries, pvAnnouncement{dest: dest, path: fwd, prefixes: route.prefixes})
		sent[dest] = true
	}
	var withdraws []uint32
	for dest := range pv.advertised[peer.id] {
		if !sent[dest] {
			withdraws = append(withdraws, dest)
		}
	}
	pv.advertised[peer.id] = sent
	pv.mu.Unlock()

	pv.sendTo(peer, entries, withdraws)
}

// pvMaxPayload is the most path-vector payload one packet may carry: the
// tun MTU. Everything rides inside WireGuard-style encryption over a
// plain UDP underlay (no DF set, no PMTU handling), so a packet bigger
// than the tun MTU + routing header would just be IP-fragmented on the
// wire -- fragile, and exactly what the MTU was sized to avoid.
func (pv *PathVector) pvMaxPayload() int {
	if mtu := int(pv.node.mtu.Load()); mtu > 0 {
		return mtu
	}
	return defaultMTU
}

func pvEntrySize(a pvAnnouncement) int { return 2 + len(a.path) + 1 + len(a.prefixes)*5 }

// sendTo validates the entries and splits them across as many packets as
// needed to stay within pvMaxPayload, like BGP splitting a large table
// across UPDATEs (RFC 4271 caps a message at 4096 bytes). Entries are
// independent (one dest each; the receiver replaces its per-neighbor
// route for a dest wholesale), so chunks need no ordering or atomicity,
// and the periodic full sync repairs any lost chunk.
func (pv *PathVector) sendTo(peer *Link, announces []pvAnnouncement, withdraws []uint32) {
	max := pv.pvMaxPayload()
	var (
		chunkA []pvAnnouncement
		chunkW []uint32
		size   = pvHeaderSize
	)
	flush := func() {
		if len(chunkA) > 0 || len(chunkW) > 0 {
			pv.sendPacket(peer, chunkA, chunkW)
		}
		chunkA, chunkW, size = nil, nil, pvHeaderSize
	}
	for _, a := range announces {
		// PathLen and NumPrefix are single bytes on the wire; refuse
		// rather than wrap (which would corrupt every entry after it).
		if len(a.path) > pvMaxPathLen || len(a.prefixes) > 255 {
			pv.node.log.Errorf("pathvector: not advertising dest %d to %v: path length %d / %d prefixes exceeds the wire format's 255 limit", a.dest, peer, len(a.path), len(a.prefixes))
			continue
		}
		es := pvEntrySize(a)
		if pvHeaderSize+es > max {
			// One dest's entry alone doesn't fit. Can't be split: the
			// receiver keeps only the last entry per dest.
			pv.node.log.Errorf("pathvector: not advertising dest %d to %v: single entry is %d bytes, over the %d-byte packet limit (too many prefixes?)", a.dest, peer, pvHeaderSize+es, max)
			continue
		}
		if size+es > max {
			flush()
		}
		chunkA = append(chunkA, a)
		size += es
	}
	for _, w := range withdraws {
		if size+1 > max {
			flush()
		}
		chunkW = append(chunkW, w)
		size++
	}
	flush()
}

// sendPacket sends one already-sized proto=2 packet.
func (pv *PathVector) sendPacket(peer *Link, announces []pvAnnouncement, withdraws []uint32) {
	if pv.sendHook != nil {
		pv.sendHook(peer, announces, withdraws)
		return
	}
	// path-vector is link-local: sent to the neighbor on this link, never forwarded.
	peer.send(pvProto, linkLocalTTL, encodePVPacket(announces, withdraws))
}

// forwardPath appends our own id to a learned path before re-advertising
// it onward toward a specific neighbor -- required so that whoever
// receives it next can check "the path's last hop is whoever's sending
// me this" (see handlePacket's route.viaNeighbor() != from.id check).
// Every hop does this at least once, which is what makes len(path) a
// correct hop count and viaNeighbor() always mean "the neighbor who
// told me this" from the local recipient's own point of view.
//
// AS-prepending (via.prependCount, config: [[link]] prependCount) adds
// extra copies beyond that mandatory one, specifically for whatever's
// being sent out over *this* link -- inflating the path length anyone
// downstream of `via` sees for routes relayed this way, without
// touching this node's own directly-originated routes or anything
// relayed over a different, unprepended link. This is deliberately the
// only lever path-vector has for traffic engineering: no separate cost
// metric, since prepending reuses the exact same shortest-path
// selection recomputeBestLocked already does for every other reason,
// per PROJECT_STATE.md's discussion of why (a plain distance-vector
// hop count can otherwise prefer a low-hop long-haul link over a
// cheaper multi-hop in-DC path).
func (pv *PathVector) forwardPath(path []uint32, via *Link) []uint32 {
	n := 1 + int(via.prependCount)
	out := make([]uint32, len(path)+n)
	copy(out, path)
	for i := 0; i < n; i++ {
		out[len(path)+i] = pv.localID
	}
	return out
}

func pathsEqual(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type pvAnnouncement struct {
	dest     uint32
	path     []uint32
	prefixes []pvPrefix
}

func encodePVPacket(announces []pvAnnouncement, withdraws []uint32) []byte {
	size := pvHeaderSize
	for _, a := range announces {
		size += 2 + len(a.path) + 1 + len(a.prefixes)*5
	}
	size += len(withdraws)

	buf := make([]byte, size)
	buf[0] = pvVers1
	binary.BigEndian.PutUint16(buf[4:6], uint16(len(announces)))
	binary.BigEndian.PutUint16(buf[6:8], uint16(len(withdraws)))

	off := pvHeaderSize
	for _, a := range announces {
		buf[off] = byte(a.dest)
		buf[off+1] = byte(len(a.path))
		off += 2
		for _, hop := range a.path {
			buf[off] = byte(hop)
			off++
		}
		buf[off] = byte(len(a.prefixes))
		off++
		for _, p := range a.prefixes {
			buf[off] = p.len
			binary.BigEndian.PutUint32(buf[off+1:off+5], p.addr)
			off += 5
		}
	}
	for _, w := range withdraws {
		buf[off] = byte(w)
		off++
	}
	binary.BigEndian.PutUint16(buf[2:4], uint16(size))
	return buf
}

func decodePVPacket(payload []byte) (announces []pvAnnouncement, withdraws []uint32, ok bool) {
	if len(payload) < pvHeaderSize {
		return nil, nil, false
	}
	numAnnounce := binary.BigEndian.Uint16(payload[4:6])
	numWithdraw := binary.BigEndian.Uint16(payload[6:8])

	off := pvHeaderSize
	for i := 0; i < int(numAnnounce); i++ {
		if off+2 > len(payload) {
			return nil, nil, false
		}
		dest := uint32(payload[off])
		pathLen := int(payload[off+1])
		off += 2
		if off+pathLen > len(payload) {
			return nil, nil, false
		}
		path := make([]uint32, pathLen)
		for j := 0; j < pathLen; j++ {
			path[j] = uint32(payload[off+j])
		}
		off += pathLen

		if off+1 > len(payload) {
			return nil, nil, false
		}
		numPrefix := int(payload[off])
		off++
		var prefixes []pvPrefix
		for j := 0; j < numPrefix; j++ {
			if off+5 > len(payload) {
				return nil, nil, false
			}
			plen := payload[off]
			if plen > 32 {
				return nil, nil, false
			}
			addr := binary.BigEndian.Uint32(payload[off+1 : off+5])
			off += 5
			prefixes = append(prefixes, pvPrefix{addr: addr, len: plen})
		}

		announces = append(announces, pvAnnouncement{dest: dest, path: path, prefixes: prefixes})
	}
	for i := 0; i < int(numWithdraw); i++ {
		if off+1 > len(payload) {
			return nil, nil, false
		}
		withdraws = append(withdraws, uint32(payload[off]))
		off++
	}
	return announces, withdraws, true
}
