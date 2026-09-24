package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// introspect.go is melnode's read-only "show ..." surface -- the
// equivalent of `vtysh -c 'show ip bgp'` / `show bfd peers` -- served as
// JSON over the same local Unix control socket as the advertise/withdraw
// API (controlapi.go). Every handler works from a point-in-time snapshot
// taken under the owning component's own lock, never holding one lock
// while taking another, so a slow client can't stall the data path.
//
//	GET /            list of endpoints
//	GET /summary     this node at a glance
//	GET /links       one entry per [[link]]: BFD-like liveness + wire stats
//	GET /routes      path-vector table: every candidate path per destination
//	GET /prefixes    advertised prefixes: claimants and the winning owner
//	GET /tuns        the per-destination tun interfaces
//
// (Try: curl --unix-socket /run/melnode/control.sock http://x/links)

var processStart = time.Now()

// ---- links ----------------------------------------------------------------

type linkInfo struct {
	PeerID                uint32   `json:"peer_id"`
	Endpoint              string   `json:"endpoint,omitempty"` // current remote address; empty for a listen-only link nobody has spoken on yet
	PublicKey             string   `json:"public_key"`
	State                 string   `json:"state"` // Down / Init / Up / AdminDown
	StateForSeconds       float64  `json:"state_for_seconds"`
	Diag                  string   `json:"diag"`
	LocalDiscriminator    uint32   `json:"local_discriminator"`
	RemoteDiscriminator   uint32   `json:"remote_discriminator"` // 0 until learned
	RemoteDesiredMinTXMs  float64  `json:"remote_desired_min_tx_ms"`
	RemoteRequiredMinRXMs float64  `json:"remote_required_min_rx_ms"`
	PrependCount          uint32   `json:"prepend_count"`
	LastHandshakeSecAgo   *float64 `json:"last_handshake_seconds_ago"` // null: never
	TxBytes               uint64   `json:"tx_bytes"`
	RxBytes               uint64   `json:"rx_bytes"`
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func (peer *Peer) linkInfo() linkInfo {
	m := peer.monitor
	m.mu.Lock()
	info := linkInfo{
		State:                 m.state.String(),
		StateForSeconds:       time.Since(m.stateSince).Seconds(),
		Diag:                  m.diag.String(),
		LocalDiscriminator:    m.localDiscriminator,
		RemoteDiscriminator:   m.remoteDiscriminator,
		RemoteDesiredMinTXMs:  ms(m.remoteDesiredMinTX),
		RemoteRequiredMinRXMs: ms(m.remoteRequiredMinRX),
	}
	m.mu.Unlock()

	info.PeerID = peer.id
	info.PrependCount = peer.prependCount
	info.TxBytes = peer.txBytes.Load()
	info.RxBytes = peer.rxBytes.Load()
	pk := peer.handshake.remoteStatic
	info.PublicKey = keyToBase64(pk[:])

	peer.endpoint.Lock()
	if peer.endpoint.val != nil {
		info.Endpoint = peer.endpoint.val.DstToString()
	}
	peer.endpoint.Unlock()

	if ns := peer.lastHandshakeNano.Load(); ns > 0 {
		ago := time.Since(time.Unix(0, ns)).Seconds()
		info.LastHandshakeSecAgo = &ago
	}
	return info
}

func (d *Device) linksSnapshot() []linkInfo {
	d.peers.RLock()
	peers := make([]*Peer, 0, len(d.peers.keyMap))
	for _, p := range d.peers.keyMap {
		peers = append(peers, p)
	}
	d.peers.RUnlock()

	out := make([]linkInfo, 0, len(peers))
	for _, p := range peers {
		out = append(out, p.linkInfo())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PeerID < out[j].PeerID })
	return out
}

// ---- tuns -----------------------------------------------------------------

type tunInfo struct {
	PeerID  uint32  `json:"peer_id"` // the destination this tun leads to
	Name    string  `json:"name"`
	MTU     int     `json:"mtu"`
	NextHop *uint32 `json:"next_hop"` // link peerid the route currently resolves to; null if none
}

func (r *Router) tunsSnapshot() []tunInfo {
	r.mu.RLock()
	tuns := make(map[uint32]tunInfoSrc, len(r.tunByPeerID))
	for id, t := range r.tunByPeerID {
		nh, ok := r.routeTable[id]
		tuns[id] = tunInfoSrc{t: t, nhid: nh, hasNH: ok}
	}
	r.mu.RUnlock()

	out := make([]tunInfo, 0, len(tuns))
	for id, src := range tuns {
		ti := tunInfo{PeerID: id}
		ti.Name, _ = src.t.Name()
		ti.MTU, _ = src.t.MTU()
		if src.hasNH {
			nh := src.nhid
			ti.NextHop = &nh
		}
		out = append(out, ti)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PeerID < out[j].PeerID })
	return out
}

type tunInfoSrc struct {
	t interface {
		Name() (string, error)
		MTU() (int, error)
	}
	nhid  uint32
	hasNH bool
}

// ---- routes ---------------------------------------------------------------

type pathInfo struct {
	Best     bool     `json:"best"`
	Via      uint32   `json:"via"`      // the neighbor that told us this path
	Path     []uint32 `json:"path"`     // nearest neighbor first, origin last (like an AS_PATH)
	Length   int      `json:"length"`   // == len(path)
	Prefixes []string `json:"prefixes"` // what the origin claims to own
}

type routeInfo struct {
	Dest  uint32     `json:"dest"`
	Tun   string     `json:"tun,omitempty"`
	Paths []pathInfo `json:"paths"`
}

func reversed(p []uint32) []uint32 {
	out := make([]uint32, len(p))
	for i, v := range p {
		out[len(p)-1-i] = v
	}
	return out
}

func prefixStrings(ps []pvPrefix) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	sort.Strings(out)
	return out
}

func (pv *PathVector) routesSnapshot() []routeInfo {
	pv.mu.Lock()
	out := make([]routeInfo, 0, len(pv.learned))
	for dest, byNeighbor := range pv.learned {
		if len(byNeighbor) == 0 {
			continue
		}
		best, haveBest := pv.best[dest]
		ri := routeInfo{Dest: dest}
		for nid, route := range byNeighbor {
			ri.Paths = append(ri.Paths, pathInfo{
				Best:     haveBest && nid == best.viaNeighbor() && pathsEqual(route.path, best.path),
				Via:      nid,
				Path:     reversed(route.path),
				Length:   len(route.path),
				Prefixes: prefixStrings(route.prefixes),
			})
		}
		sort.Slice(ri.Paths, func(i, j int) bool {
			a, b := ri.Paths[i], ri.Paths[j]
			if a.Best != b.Best {
				return a.Best
			}
			if a.Length != b.Length {
				return a.Length < b.Length
			}
			return a.Via < b.Via
		})
		out = append(out, ri)
	}
	pv.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Dest < out[j].Dest })
	names := make(map[uint32]string)
	for _, t := range pv.dev.router.tunsSnapshot() {
		names[t.PeerID] = t.Name
	}
	for i := range out {
		out[i].Tun = names[out[i].Dest]
	}
	return out
}

// ---- prefixes -------------------------------------------------------------

type prefixInfo struct {
	Prefix     string   `json:"prefix"`
	Owner      *uint32  `json:"owner"` // winning claimant; null if unowned
	OwnerLocal bool     `json:"owner_is_local"`
	PathLength int      `json:"path_length"` // hops to the owner; 0 if local
	Claimants  []uint32 `json:"claimants"`
	Local      bool     `json:"locally_advertised"`
}

func (pv *PathVector) prefixesSnapshot() []prefixInfo {
	pv.mu.Lock()
	seen := make(map[pvPrefix]bool, len(pv.prefixClaims)+len(pv.localPrefixes))
	for p := range pv.prefixClaims {
		seen[p] = true
	}
	for p := range pv.localPrefixes {
		seen[p] = true
	}
	type item struct {
		p pvPrefix
		i prefixInfo
	}
	items := make([]item, 0, len(seen))
	for p := range seen {
		info := prefixInfo{Prefix: p.String(), Local: pv.localPrefixes[p], Claimants: []uint32{}}
		for id := range pv.prefixClaims[p] {
			info.Claimants = append(info.Claimants, id)
		}
		sort.Slice(info.Claimants, func(a, b int) bool { return info.Claimants[a] < info.Claimants[b] })
		if owner, ok := pv.recomputePrefixOwnerLocked(p); ok {
			o := owner
			info.Owner = &o
			info.OwnerLocal = owner == pv.localID
			if !info.OwnerLocal {
				info.PathLength = len(pv.best[owner].path)
			}
		}
		items = append(items, item{p, info})
	}
	pv.mu.Unlock()

	sort.Slice(items, func(i, j int) bool {
		if items[i].p.addr != items[j].p.addr {
			return items[i].p.addr < items[j].p.addr
		}
		return items[i].p.len < items[j].p.len
	})
	out := make([]prefixInfo, len(items))
	for i, it := range items {
		out[i] = it.i
	}
	return out
}

// ---- summary --------------------------------------------------------------

type summaryInfo struct {
	LocalID        uint32  `json:"local_id"`
	PublicKey      string  `json:"public_key"`
	UptimeSeconds  float64 `json:"uptime_seconds"`
	MTU            int     `json:"mtu"`
	IdentityPrefix string  `json:"identity_prefix,omitempty"`
	LinksTotal     int     `json:"links_total"`
	LinksUp        int     `json:"links_up"`
	Destinations   int     `json:"destinations"` // peerids currently reachable
	Tuns           int     `json:"tuns"`
	Prefixes       int     `json:"prefixes"`
}

func (pv *PathVector) summarySnapshot() summaryInfo {
	d := pv.dev
	s := summaryInfo{
		LocalID:       d.localID,
		UptimeSeconds: time.Since(processStart).Seconds(),
		MTU:           d.mtu,
	}
	d.staticIdentity.RLock()
	s.PublicKey = keyToBase64(d.staticIdentity.publicKey[:])
	d.staticIdentity.RUnlock()
	if d.router != nil && d.router.identityPrefix != nil {
		s.IdentityPrefix = d.router.identityPrefix.String()
	}
	links := d.linksSnapshot()
	s.LinksTotal = len(links)
	for _, l := range links {
		if l.State == linkStateUp.String() {
			s.LinksUp++
		}
	}
	s.Tuns = len(d.router.tunsSnapshot())
	pv.mu.Lock()
	s.Destinations = len(pv.best)
	s.Prefixes = len(pv.prefixClaims)
	pv.mu.Unlock()
	return s
}

// ---- HTTP -----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func getOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

// registerIntrospection mounts the read-only endpoints on the control API's mux.
func (c *controlAPI) registerIntrospection(mux *http.ServeMux) {
	pv := c.pv
	mux.HandleFunc("/summary", getOnly(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, pv.summarySnapshot()) }))
	mux.HandleFunc("/links", getOnly(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, pv.dev.linksSnapshot()) }))
	mux.HandleFunc("/routes", getOnly(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, pv.routesSnapshot()) }))
	mux.HandleFunc("/prefixes", getOnly(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, pv.prefixesSnapshot()) }))
	mux.HandleFunc("/tuns", getOnly(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, pv.dev.router.tunsSnapshot()) }))
	mux.HandleFunc("/", getOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{
			"read": []string{"/summary", "/links", "/routes", "/prefixes", "/tuns"},
			"write": map[string]string{
				"/advertise": `POST {"prefix": "a.b.c.d/n"}`,
				"/withdraw":  `POST {"prefix": "a.b.c.d/n"}`,
			},
		})
	}))
}
