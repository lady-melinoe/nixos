package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	pvProto = 2
	pvVers1 = 1

	pvHeaderSize = 8

	pvFullSyncInterval = 30 * time.Second

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

type PathVector struct {
	node    *Node
	localID uint32

	tblMu sync.Mutex

	sendMu map[uint32]*sync.Mutex

	mu      sync.Mutex
	learned map[uint32]map[uint32]pvRoute
	best    map[uint32]pvRoute

	neighbors map[uint32]*Link

	advertised map[uint32]map[uint32]bool

	dirty map[uint32]map[uint32]bool

	localPrefixes map[pvPrefix]bool

	prefixClaims map[pvPrefix]map[uint32]bool
	prefixOwner  map[pvPrefix]uint32

	stopCh chan struct{}
	wg     sync.WaitGroup

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
		dirty:         make(map[uint32]map[uint32]bool),
		sendMu:        make(map[uint32]*sync.Mutex),
		localPrefixes: make(map[pvPrefix]bool),
		prefixClaims:  make(map[pvPrefix]map[uint32]bool),
		prefixOwner:   make(map[pvPrefix]uint32),
	}
	if node.router != nil {
		node.router.desiredPrefixes = pv.snapshotPrefixOwners
	}
	return pv
}

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
			pv.syncKernel()
		}
	}
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
	pv.neighbors[peer.id] = peer
	delete(pv.advertised, peer.id)
	delete(pv.dirty, peer.id)
	pv.mu.Unlock()
	pv.node.log.Verbosef("pathvector: neighbor %v up, sending full table", peer)
	pv.fullSyncTo(peer)
}

func (pv *PathVector) OnLinkDown(peer *Link) {
	pv.mu.Lock()
	delete(pv.neighbors, peer.id)
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
	announces, withdraws, ok := decodePVPacket(payload)
	if !ok {
		pv.node.log.Verbosef("%v - pathvector: malformed packet, dropping", from)
		return
	}

	pv.mu.Lock()
	if !pv.acceptingFromLocked(from) {
		pv.mu.Unlock()
		pv.node.log.Verbosef("%v - pathvector: link not up, dropping", from)
		return
	}
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
	neighbors := pv.neighborsSnapshotLocked()
	pv.mu.Unlock()

	pv.flushAll(neighbors)
	if changed || len(prefixChanges) > 0 {
		pv.syncKernel()
	}
}

func (pv *PathVector) acceptingFromLocked(from *Link) bool {
	if _, ok := pv.neighbors[from.id]; ok {
		return true
	}
	if from.monitor == nil {
		return false
	}
	st := from.monitor.State()
	return st == linkStateInit || st == linkStateUp
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
		return pvAnnouncement{dest: dest, path: []uint32{pv.localID}, prefixes: pv.localPrefixesListLocked()}, true
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
	dirty := pv.dirty[peer.id]
	delete(pv.dirty, peer.id)
	if len(dirty) == 0 {
		pv.mu.Unlock()
		return
	}
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
	pv.mu.Unlock()

	pv.sendTo(peer, announces, withdraws)
}

func (pv *PathVector) flushAll(neighbors []*Link) {
	for _, n := range neighbors {
		pv.flushTo(n)
	}
}

func (pv *PathVector) fullSyncTo(peer *Link) {
	pv.mu.Lock()
	d := pv.dirty[peer.id]
	if d == nil {
		d = make(map[uint32]bool)
		pv.dirty[peer.id] = d
	}
	d[pv.localID] = true
	for dest := range pv.best {
		d[dest] = true
	}
	for dest := range pv.advertised[peer.id] {
		d[dest] = true
	}
	pv.mu.Unlock()

	pv.flushTo(peer)
}

func (pv *PathVector) pvMaxPayload() int {
	if mtu := int(pv.node.mtu.Load()); mtu > 0 {
		return mtu
	}
	return defaultMTU
}

func pvEntrySize(a pvAnnouncement) int { return 2 + len(a.path) + 1 + len(a.prefixes)*5 }

func pvEntrySendable(a pvAnnouncement, max int) bool {
	return len(a.path) <= pvMaxPathLen && len(a.prefixes) <= 255 && pvHeaderSize+pvEntrySize(a) <= max
}

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

func (pv *PathVector) sendPacket(peer *Link, announces []pvAnnouncement, withdraws []uint32) {
	if pv.sendHook != nil {
		pv.sendHook(peer, announces, withdraws)
		return
	}
	peer.send(pvProto, linkLocalTTL, encodePVPacket(announces, withdraws))
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
			return nil, nil, false
		}
		withdraws = append(withdraws, uint32(payload[off]))
		off++
	}
	return announces, withdraws, true
}
