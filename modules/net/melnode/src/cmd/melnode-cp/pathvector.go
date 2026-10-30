package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"melnode/dpproto"
)

const (
	pvProto = 2
	pvVers2 = 2

	pvHeaderSize = 24

	pvTimerTick          = 50 * time.Millisecond
	pvKernelSyncInterval = 30 * time.Second

	pvRTOInitial     = 250 * time.Millisecond
	pvRTOMax         = 4 * time.Second
	pvMaxRetransmits = 10
	pvWindow         = 64

	pvMaxPathLen = 255
)

type pvPrefix struct {
	addr uint32
	len  uint8
}

func (p pvPrefix) String() string {
	return fmt.Sprintf("%d.%d.%d.%d/%d", byte(p.addr>>24), byte(p.addr>>16), byte(p.addr>>8), byte(p.addr), p.len)
}

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
	if bits != 32 || !ip4.Equal(ipnet.IP.To4()) {
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

type pvRoute struct {
	path     []uint32
	prefixes []pvPrefix
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

type prefixOwnerChange struct {
	prefix pvPrefix
	owner  uint32
	ok     bool
}

type pvHeader struct {
	sendSess uint32
	recvSess uint32
	seq      uint32
	ack      uint32
}

type pvSegment struct {
	seq       uint32
	announces []pvAnnouncement
	withdraws []uint32
}

type pvSession struct {
	local        uint32
	remote       uint32
	nextSeq      uint32
	expect       uint32
	unacked      []pvSegment
	rto          time.Duration
	retransmitAt time.Time
	retries      int
	ackPending   bool
	failed       bool
}

func newPVSession() *pvSession {
	return &pvSession{local: randomSessionID(), nextSeq: 1, expect: 1, rto: pvRTOInitial}
}

func randomSessionID() uint32 {
	var id uint32
	for id == 0 {
		var b [4]byte
		_, _ = rand.Read(b[:])
		id = binary.BigEndian.Uint32(b[:])
	}
	return id
}

func (s *pvSession) header(seq uint32) pvHeader {
	return pvHeader{sendSess: s.local, recvSess: s.remote, seq: seq, ack: s.expect - 1}
}

type PathVector struct {
	node    *Node
	localID uint32

	tblMu sync.Mutex

	sendMu map[uint32]*sync.Mutex

	mu      sync.Mutex
	learned map[uint32]map[uint32]pvRoute
	best    map[uint32]pvRoute

	neighbors map[uint32]*Link

	sessions map[uint32]*pvSession

	advertised map[uint32]map[uint32]bool

	dirty map[uint32]map[uint32]bool

	localPrefixes map[pvPrefix]bool

	prefixClaims map[pvPrefix]map[uint32]bool
	prefixOwner  map[pvPrefix]uint32

	stopCh chan struct{}
	wg     sync.WaitGroup

	sendHook func(peer *Link, wire []byte)
	failLink func(peer *Link)
}

func newPathVector(node *Node, localID uint32) *PathVector {
	pv := &PathVector{
		node:          node,
		localID:       localID,
		learned:       make(map[uint32]map[uint32]pvRoute),
		best:          make(map[uint32]pvRoute),
		neighbors:     make(map[uint32]*Link),
		sessions:      make(map[uint32]*pvSession),
		advertised:    make(map[uint32]map[uint32]bool),
		dirty:         make(map[uint32]map[uint32]bool),
		sendMu:        make(map[uint32]*sync.Mutex),
		localPrefixes: make(map[pvPrefix]bool),
		prefixClaims:  make(map[pvPrefix]map[uint32]bool),
		prefixOwner:   make(map[pvPrefix]uint32),
	}
	pv.failLink = pv.resetLink
	if node.router != nil {
		node.router.desiredPrefixes = pv.snapshotPrefixOwners
	}
	return pv
}

func (pv *PathVector) Start() {
	pv.stopCh = make(chan struct{})
	pv.node.router.StartReconciler(pv.stopCh, &pv.wg)
	pv.wg.Add(1)
	go pv.timerLoop()
}

func (pv *PathVector) Stop() {
	close(pv.stopCh)
	pv.wg.Wait()
}

func (pv *PathVector) timerLoop() {
	defer pv.wg.Done()
	tick := time.NewTicker(pvTimerTick)
	defer tick.Stop()
	kernel := time.NewTicker(pvKernelSyncInterval)
	defer kernel.Stop()
	for {
		select {
		case <-pv.stopCh:
			return
		case now := <-tick.C:
			pv.retransmitDue(now)
		case <-kernel.C:
			pv.syncKernel()
		}
	}
}

func (pv *PathVector) retransmitDue(now time.Time) {
	pv.mu.Lock()
	neighbors := pv.neighborsSnapshotLocked()
	pv.mu.Unlock()
	var failed []*Link
	for _, n := range neighbors {
		if pv.retransmitTo(n, now) {
			failed = append(failed, n)
		}
	}
	for _, n := range failed {
		pv.node.log.Errorf("pathvector: %v did not acknowledge updates after %d retransmits, resetting the link", n, pvMaxRetransmits)
		pv.failLink(n)
	}
}

func (pv *PathVector) retransmitTo(peer *Link, now time.Time) (failed bool) {
	l := pv.sendLock(peer.id)
	l.Lock()
	defer l.Unlock()

	pv.mu.Lock()
	s := pv.sessions[peer.id]
	if s == nil || s.failed || len(s.unacked) == 0 || now.Before(s.retransmitAt) {
		pv.mu.Unlock()
		return false
	}
	if s.retries >= pvMaxRetransmits {
		s.failed = true
		pv.mu.Unlock()
		return true
	}
	s.retries++
	s.rto = min(s.rto*2, pvRTOMax)
	s.retransmitAt = now.Add(s.rto)
	wires := make([][]byte, 0, len(s.unacked))
	for _, seg := range s.unacked {
		wires = append(wires, encodePVPacket(s.header(seg.seq), seg.announces, seg.withdraws))
	}
	s.ackPending = false
	attempt := s.retries
	pv.mu.Unlock()

	pv.node.log.Verbosef("pathvector: retransmitting %d unacknowledged updates to %v (attempt %d)", len(wires), peer, attempt)
	for _, w := range wires {
		pv.sendPacket(peer, w)
	}
	return false
}

func (pv *PathVector) resetLink(peer *Link) {
	if peer.monitor != nil {
		peer.monitor.transitionTo(linkStateDown, diagPathVectorTimeout)
		return
	}
	pv.OnLinkDown(peer)
}

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

func (pv *PathVector) snapshotPrefixOwners() map[pvPrefix]uint32 {
	pv.mu.Lock()
	defer pv.mu.Unlock()
	out := make(map[pvPrefix]uint32, len(pv.prefixOwner))
	for p, o := range pv.prefixOwner {
		out[p] = o
	}
	return out
}

func (pv *PathVector) OnLinkUp(peer *Link) {
	pv.mu.Lock()
	_, stale := pv.sessions[peer.id]
	pv.mu.Unlock()
	if stale {
		pv.OnLinkDown(peer)
	}

	pv.mu.Lock()
	pv.neighbors[peer.id] = peer
	pv.sessions[peer.id] = newPVSession()
	delete(pv.advertised, peer.id)
	d := make(map[uint32]bool, len(pv.best)+1)
	d[pv.localID] = true
	for dest := range pv.best {
		d[dest] = true
	}
	pv.dirty[peer.id] = d
	pv.mu.Unlock()
	pv.node.log.Verbosef("pathvector: neighbor %v up, sending full table", peer)
	pv.flushTo(peer)
}

func (pv *PathVector) OnLinkDown(peer *Link) {
	pv.mu.Lock()
	delete(pv.neighbors, peer.id)
	delete(pv.sessions, peer.id)
	delete(pv.advertised, peer.id)
	delete(pv.dirty, peer.id)

	changed := false
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
			if hadOld {
				changed = true
				pv.markDirtyLocked(dest)
				for p := range pv.updatePrefixClaimsLocked(dest, old.prefixes, nil) {
					affectedPrefixes[p] = true
				}
			}
		} else if !hadOld || !pathsEqual(old.path, newBest.path) || !prefixesEqual(old.prefixes, newBest.prefixes) {
			pv.best[dest] = newBest
			changed = true
			pv.markDirtyLocked(dest)
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

	pv.flushAll(neighbors)
	if changed || len(prefixChanges) > 0 {
		pv.syncKernel()
	}
}

func (pv *PathVector) handlePacket(from *Link, payload []byte) {
	hdr, announces, withdraws, ok := decodePVPacket(payload)
	if !ok {
		pv.node.log.Verbosef("%v - pathvector: malformed packet, dropping", from)
		return
	}

	pv.mu.Lock()
	s, why := pv.sessionForLocked(from, hdr)
	if s == nil {
		pv.mu.Unlock()
		pv.node.log.Verbosef("%v - pathvector: %s, dropping", from, why)
		return
	}
	pv.ackLocked(s, hdr.ack, time.Now())
	inOrder := false
	if hdr.seq != 0 {
		s.ackPending = true
		if hdr.seq == s.expect {
			s.expect++
			inOrder = true
		}
	}
	syncNeeded := false
	if inOrder {
		syncNeeded = pv.applyUpdatesLocked(from, announces, withdraws)
	}
	neighbors := pv.neighborsSnapshotLocked()
	pv.mu.Unlock()

	pv.flushAll(neighbors)
	if syncNeeded {
		pv.syncKernel()
	}
}

func (pv *PathVector) sessionForLocked(from *Link, h pvHeader) (*pvSession, string) {
	s := pv.sessions[from.id]
	switch {
	case s == nil:
		return nil, "no session (link not up)"
	case h.sendSess == 0:
		return nil, "missing session id"
	case h.recvSess != 0 && h.recvSess != s.local:
		return nil, "addressed to a previous session"
	case s.remote != 0 && h.sendSess != s.remote:
		return nil, "from a different peer session"
	}
	s.remote = h.sendSess
	return s, ""
}

func (pv *PathVector) ackLocked(s *pvSession, ack uint32, now time.Time) {
	if ack == 0 || ack >= s.nextSeq {
		return
	}
	n := 0
	for n < len(s.unacked) && s.unacked[n].seq <= ack {
		n++
	}
	if n == 0 {
		return
	}
	s.unacked = append([]pvSegment(nil), s.unacked[n:]...)
	s.retries = 0
	s.rto = pvRTOInitial
	if len(s.unacked) > 0 {
		s.retransmitAt = now.Add(s.rto)
	} else {
		s.retransmitAt = time.Time{}
	}
}

func (pv *PathVector) applyUpdatesLocked(from *Link, announces []pvAnnouncement, withdraws []uint32) bool {
	changed := false
	affectedPrefixes := make(map[pvPrefix]bool)

	for _, a := range announces {
		if a.dest == pv.localID {
			continue
		}
		if len(a.path) == 0 || len(a.path) > pvMaxPathLen {
			continue
		}
		route := pvRoute{path: a.path, prefixes: a.prefixes}
		if route.contains(pv.localID) {
			continue
		}
		if route.viaNeighbor() != from.id {
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
			changed = true
			pv.markDirtyLocked(a.dest)
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
				changed = true
				pv.markDirtyLocked(dest)
				for p := range pv.updatePrefixClaimsLocked(dest, old.prefixes, nil) {
					affectedPrefixes[p] = true
				}
			}
		} else if !hadOld || !pathsEqual(old.path, newBest.path) || !prefixesEqual(old.prefixes, newBest.prefixes) {
			pv.best[dest] = newBest
			changed = true
			pv.markDirtyLocked(dest)
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
	return changed || len(prefixChanges) > 0
}

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
	pv.markDirtyLocked(pv.localID)
	neighbors := pv.neighborsSnapshotLocked()
	pv.mu.Unlock()

	if advertise {
		pv.node.log.Verbosef("pathvector: now advertising %v", p)
	} else {
		pv.node.log.Verbosef("pathvector: no longer advertising %v", p)
	}
	if len(prefixChanges) > 0 {
		pv.syncKernel()
	}
	pv.flushAll(neighbors)
}

func (pv *PathVector) localPrefixesListLocked() []pvPrefix {
	out := make([]pvPrefix, 0, len(pv.localPrefixes))
	for p := range pv.localPrefixes {
		out = append(out, p)
	}
	return out
}

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
				continue
			}
			pathLen = len(route.path)
		}
		if !found || pathLen < bestLen || (pathLen == bestLen && id < bestID) {
			bestID, bestLen, found = id, pathLen, true
		}
	}
	return bestID, found
}

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

func (pv *PathVector) markDirtyLocked(dest uint32) {
	for nid := range pv.neighbors {
		d := pv.dirty[nid]
		if d == nil {
			d = make(map[uint32]bool)
			pv.dirty[nid] = d
		}
		d[dest] = true
	}
}

func (pv *PathVector) entryForLocked(dest uint32, n *Link) (pvAnnouncement, bool) {
	if dest == pv.localID {
		return pvAnnouncement{dest: dest, path: pv.forwardPath(nil, n), prefixes: pv.localPrefixesListLocked()}, true
	}
	route, ok := pv.best[dest]
	if !ok || route.contains(n.id) {
		return pvAnnouncement{}, false
	}
	fwd := pv.forwardPath(route.path, n)
	if len(fwd) > pvMaxPathLen {
		return pvAnnouncement{}, false
	}
	return pvAnnouncement{dest: dest, path: fwd, prefixes: route.prefixes}, true
}

func (pv *PathVector) sendLock(nid uint32) *sync.Mutex {
	pv.mu.Lock()
	defer pv.mu.Unlock()
	l := pv.sendMu[nid]
	if l == nil {
		l = &sync.Mutex{}
		pv.sendMu[nid] = l
	}
	return l
}

func (pv *PathVector) flushTo(peer *Link) {
	l := pv.sendLock(peer.id)
	l.Lock()
	defer l.Unlock()

	max := pv.pvMaxPayload()
	pv.mu.Lock()
	s := pv.sessions[peer.id]
	if s == nil || s.failed {
		pv.mu.Unlock()
		return
	}
	var wires [][]byte
	if dirty := pv.dirty[peer.id]; len(dirty) > 0 && len(s.unacked) < pvWindow {
		delete(pv.dirty, peer.id)
		announces, withdraws := pv.updatesForLocked(peer, dirty, max)
		for _, seg := range pv.segment(peer, announces, withdraws, max) {
			seg.seq = s.nextSeq
			s.nextSeq++
			s.unacked = append(s.unacked, seg)
			wires = append(wires, encodePVPacket(s.header(seg.seq), seg.announces, seg.withdraws))
		}
		if len(wires) > 0 && s.retransmitAt.IsZero() {
			s.retransmitAt = time.Now().Add(s.rto)
		}
	}
	if len(wires) == 0 && s.ackPending {
		wires = append(wires, encodePVPacket(s.header(0), nil, nil))
	}
	s.ackPending = false
	pv.mu.Unlock()

	for _, w := range wires {
		pv.sendPacket(peer, w)
	}
}

func (pv *PathVector) updatesForLocked(peer *Link, dirty map[uint32]bool, max int) ([]pvAnnouncement, []uint32) {
	dests := make([]uint32, 0, len(dirty))
	for dest := range dirty {
		dests = append(dests, dest)
	}
	sort.Slice(dests, func(i, j int) bool { return dests[i] < dests[j] })

	adv := pv.advertised[peer.id]
	if adv == nil {
		adv = make(map[uint32]bool)
		pv.advertised[peer.id] = adv
	}
	var announces []pvAnnouncement
	var withdraws []uint32
	for _, dest := range dests {
		a, ok := pv.entryForLocked(dest, peer)
		if ok && pvEntrySendable(a, max) {
			adv[dest] = true
			announces = append(announces, a)
			continue
		}
		if ok {
			announces = append(announces, a)
		}
		if adv[dest] {
			delete(adv, dest)
			withdraws = append(withdraws, dest)
		}
	}
	return announces, withdraws
}

func (pv *PathVector) flushAll(neighbors []*Link) {
	for _, n := range neighbors {
		pv.flushTo(n)
	}
}

func (pv *PathVector) pvMaxPayload() int {
	mtu := defaultMTU
	if m := int(pv.node.mtu.Load()); m > 0 {
		mtu = m
	}
	return min(mtu, dpproto.MaxControlPayload)
}

func pvEntrySize(a pvAnnouncement) int { return 2 + len(a.path) + 1 + len(a.prefixes)*5 }

func pvEntrySendable(a pvAnnouncement, max int) bool {
	return len(a.path) <= pvMaxPathLen && len(a.prefixes) <= 255 && pvHeaderSize+pvEntrySize(a) <= max
}

func (pv *PathVector) segment(peer *Link, announces []pvAnnouncement, withdraws []uint32, max int) []pvSegment {
	var (
		out  []pvSegment
		cur  pvSegment
		size = pvHeaderSize
	)
	flush := func() {
		if len(cur.announces) > 0 || len(cur.withdraws) > 0 {
			out = append(out, cur)
		}
		cur, size = pvSegment{}, pvHeaderSize
	}
	for _, a := range announces {
		if len(a.path) > pvMaxPathLen || len(a.prefixes) > 255 {
			pv.node.log.Errorf("pathvector: not advertising dest %d to %v: path length %d / %d prefixes exceeds the wire format's 255 limit", a.dest, peer, len(a.path), len(a.prefixes))
			continue
		}
		es := pvEntrySize(a)
		if pvHeaderSize+es > max {
			pv.node.log.Errorf("pathvector: not advertising dest %d to %v: single entry is %d bytes, over the %d-byte packet limit (too many prefixes?)", a.dest, peer, pvHeaderSize+es, max)
			continue
		}
		if size+es > max {
			flush()
		}
		cur.announces = append(cur.announces, a)
		size += es
	}
	for _, w := range withdraws {
		if size+1 > max {
			flush()
		}
		cur.withdraws = append(cur.withdraws, w)
		size++
	}
	flush()
	return out
}

func (pv *PathVector) sendPacket(peer *Link, wire []byte) {
	if pv.sendHook != nil {
		pv.sendHook(peer, wire)
		return
	}
	peer.send(pvProto, linkLocalTTL, wire)
}

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

func encodePVPacket(h pvHeader, announces []pvAnnouncement, withdraws []uint32) []byte {
	size := pvHeaderSize
	for _, a := range announces {
		size += pvEntrySize(a)
	}
	size += len(withdraws)

	buf := make([]byte, size)
	buf[0] = pvVers2
	binary.BigEndian.PutUint16(buf[2:4], uint16(size))
	binary.BigEndian.PutUint32(buf[4:8], h.sendSess)
	binary.BigEndian.PutUint32(buf[8:12], h.recvSess)
	binary.BigEndian.PutUint32(buf[12:16], h.seq)
	binary.BigEndian.PutUint32(buf[16:20], h.ack)
	binary.BigEndian.PutUint16(buf[20:22], uint16(len(announces)))
	binary.BigEndian.PutUint16(buf[22:24], uint16(len(withdraws)))

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
	return buf
}

func decodePVPacket(payload []byte) (h pvHeader, announces []pvAnnouncement, withdraws []uint32, ok bool) {
	if len(payload) < pvHeaderSize || payload[0] != pvVers2 {
		return pvHeader{}, nil, nil, false
	}
	h = pvHeader{
		sendSess: binary.BigEndian.Uint32(payload[4:8]),
		recvSess: binary.BigEndian.Uint32(payload[8:12]),
		seq:      binary.BigEndian.Uint32(payload[12:16]),
		ack:      binary.BigEndian.Uint32(payload[16:20]),
	}
	numAnnounce := binary.BigEndian.Uint16(payload[20:22])
	numWithdraw := binary.BigEndian.Uint16(payload[22:24])

	off := pvHeaderSize
	for i := 0; i < int(numAnnounce); i++ {
		if off+2 > len(payload) {
			return pvHeader{}, nil, nil, false
		}
		dest := uint32(payload[off])
		pathLen := int(payload[off+1])
		off += 2
		if off+pathLen > len(payload) {
			return pvHeader{}, nil, nil, false
		}
		path := make([]uint32, pathLen)
		for j := 0; j < pathLen; j++ {
			path[j] = uint32(payload[off+j])
		}
		off += pathLen

		if off+1 > len(payload) {
			return pvHeader{}, nil, nil, false
		}
		numPrefix := int(payload[off])
		off++
		var prefixes []pvPrefix
		for j := 0; j < numPrefix; j++ {
			if off+5 > len(payload) {
				return pvHeader{}, nil, nil, false
			}
			plen := payload[off]
			if plen > 32 {
				return pvHeader{}, nil, nil, false
			}
			addr := binary.BigEndian.Uint32(payload[off+1 : off+5])
			if plen < 32 {
				addr &^= 0xffffffff >> plen
			}
			off += 5
			prefixes = append(prefixes, pvPrefix{addr: addr, len: plen})
		}

		announces = append(announces, pvAnnouncement{dest: dest, path: path, prefixes: prefixes})
	}
	for i := 0; i < int(numWithdraw); i++ {
		if off+1 > len(payload) {
			return pvHeader{}, nil, nil, false
		}
		withdraws = append(withdraws, uint32(payload[off]))
		off++
	}
	return h, announces, withdraws, true
}
