package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"melnode/dpproto"
)

// introspect.go is melnode's read-only "show ..." surface -- the
// equivalent of `vtysh -c 'show ip bgp'` / `show bfd peers` -- served as
// JSON over the same local Unix control socket as the advertise/withdraw
// API (controlapi.go), live on every request, and over TCP from a snapshot
// refreshed on a timer (introspectapi.go). Both build every view from one
// read of the data plane (readDP) plus point-in-time copies taken under
// the owning component's own lock, never holding one lock while taking
// another, so a slow client can't stall the data path.
//
//	GET /            list of endpoints
//	GET /summary     this node at a glance
//	GET /links       one entry per [[link]]: BFD-like liveness + wire stats
//	GET /routes      path-vector table: every candidate path per destination
//	GET /prefixes    advertised prefixes: claimants and the winning owner
//	GET /tuns        the per-destination tun interfaces
//	GET /dataplane   the data plane's own counters (drops by cause, punts, injects)
//
// (Try: curl --unix-socket /run/melnode/control.sock http://x/links)

var processStart = time.Now()

// dpView is one read of the data plane's state, shared by every view built
// from it (buildViews) so they agree with each other. A nil field means
// unknown: no data plane attached, introspection busy, or that call failed.
type dpView struct {
	attached bool
	links    map[uint32]dpproto.LinkInfo
	tuns     []dpproto.TunInfo
	routes   map[uint32]uint32 // dst -> next hop
	stats    []dpproto.Stat
	err      error // the first call that failed, if any
}

// readDP reads the data plane through the introspection gate
// (introspectDP). With wait, it queues for the gate instead of giving up
// when another query holds it.
func (n *Node) readDP(wait bool) dpView {
	v := dpView{attached: n.dp() != nil}
	var cl dpproto.Datapath
	var release func()
	if wait {
		cl, release = n.introspectDPWait()
	} else {
		cl, release = n.introspectDP()
	}
	defer release()
	if cl == nil {
		return v
	}
	ok := func(err error) bool {
		if err != nil && v.err == nil {
			v.err = err
		}
		return err == nil
	}
	if list, err := cl.LinkList(); ok(err) {
		v.links = make(map[uint32]dpproto.LinkInfo, len(list))
		for _, li := range list {
			v.links[li.PeerID] = li
		}
	}
	if tuns, err := cl.TunList(); ok(err) {
		v.tuns = tuns
	}
	if rl, err := cl.RouteList(); ok(err) {
		v.routes = make(map[uint32]uint32, len(rl))
		for _, rt := range rl {
			v.routes[rt.Dst] = rt.NextHop
		}
	}
	if stats, err := cl.Stats(); ok(err) {
		v.stats = stats
	}
	return v
}

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

// linkInfo combines what the control plane knows (liveness session, config)
// with what the data plane knows (current endpoint, handshake time, byte
// counters -- dp is that link's entry from LinkList, nil if the data plane
// is detached or doesn't have it).
func (l *Link) linkInfo(dp *dpproto.LinkInfo) linkInfo {
	m := l.monitor
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

	info.PeerID = l.id
	info.PrependCount = l.prependCount
	info.PublicKey = keyToBase64(l.pubkey[:])
	info.Endpoint = l.endpoint // the configured one, until the data plane says otherwise
	if dp != nil {
		info.TxBytes = dp.TxBytes
		info.RxBytes = dp.RxBytes
		if dp.Endpoint != "" {
			info.Endpoint = dp.Endpoint
		}
		if dp.LastHandshakeUnixNano > 0 {
			ago := time.Since(time.Unix(0, dp.LastHandshakeUnixNano)).Seconds()
			info.LastHandshakeSecAgo = &ago
		}
	}
	return info
}

func (n *Node) linksFrom(v dpView) []linkInfo {
	out := make([]linkInfo, 0, len(n.links))
	for _, l := range n.links {
		var dp *dpproto.LinkInfo
		if li, ok := v.links[l.id]; ok {
			dp = &li
		}
		out = append(out, l.linkInfo(dp))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PeerID < out[j].PeerID })
	return out
}

func keyToBase64(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

// ---- tuns -----------------------------------------------------------------

type tunInfo struct {
	PeerID  uint32  `json:"peer_id"` // the destination this tun leads to
	Name    string  `json:"name"`
	MTU     int     `json:"mtu"`
	NextHop *uint32 `json:"next_hop"` // link peerid the data plane currently forwards this over; null if none
	Started bool    `json:"started"`  // false only in the moment between the tun being created and configured
}

// tunsFrom lists the data plane's tuns with the next hop it currently
// forwards each over. Empty while no data plane is attached.
func tunsFrom(v dpView) []tunInfo {
	out := make([]tunInfo, 0, len(v.tuns))
	for _, t := range v.tuns {
		ti := tunInfo{PeerID: t.PeerID, Name: t.Name, MTU: int(t.MTU), Started: t.Started}
		if nh, ok := v.routes[t.PeerID]; ok {
			ti.NextHop = &nh
		}
		out = append(out, ti)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PeerID < out[j].PeerID })
	return out
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

func (pv *PathVector) routesFrom(tuns []tunInfo) []routeInfo {
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
	for _, t := range tuns {
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
	LocalID           uint32    `json:"local_id"`
	GeneratedAt       time.Time `json:"generated_at"` // when this was read (the TCP API serves snapshots, see introspectapi.go)
	PublicKey         string    `json:"public_key"`   // empty until the data plane has been attached at least once
	DataplaneAttached bool      `json:"dataplane_attached"`
	UptimeSeconds     float64   `json:"uptime_seconds"`
	MTU               int       `json:"mtu"`
	IdentityPrefix    string    `json:"identity_prefix,omitempty"`
	LinksTotal        int       `json:"links_total"`
	LinksUp           int       `json:"links_up"`
	Destinations      int       `json:"destinations"` // peerids currently reachable
	Tuns              int       `json:"tuns"`
	Prefixes          int       `json:"prefixes"`
}

func (pv *PathVector) summaryFrom(v dpView, links []linkInfo, tuns []tunInfo, at time.Time) summaryInfo {
	n := pv.node
	s := summaryInfo{
		LocalID:           n.localID,
		GeneratedAt:       at,
		UptimeSeconds:     at.Sub(processStart).Seconds(),
		MTU:               int(n.mtu.Load()),
		DataplaneAttached: v.attached,
		LinksTotal:        len(links),
		Tuns:              len(tuns),
	}
	if pk := n.pubkey.Load(); pk != nil {
		s.PublicKey = keyToBase64(pk[:])
	}
	if n.router != nil && n.router.identityPrefix != nil {
		s.IdentityPrefix = n.router.identityPrefix.String()
	}
	for _, l := range links {
		if l.State == linkStateUp.String() {
			s.LinksUp++
		}
	}
	pv.mu.Lock()
	s.Destinations = len(pv.best)
	s.Prefixes = len(pv.prefixClaims)
	pv.mu.Unlock()
	return s
}

// ---- data plane -------------------------------------------------------------

type dataplaneInfo struct {
	Attached bool              `json:"attached"`
	Stats    map[string]uint64 `json:"stats,omitempty"` // the data plane's own counters, by name
}

// dataplaneFrom dumps the data plane's counters (drops by cause, control
// packets punted/injected). Empty while no data plane is attached.
func dataplaneFrom(v dpView) dataplaneInfo {
	info := dataplaneInfo{Attached: v.attached}
	if v.stats == nil {
		return info
	}
	info.Stats = make(map[string]uint64, len(v.stats))
	for _, s := range v.stats {
		name, ok := dpproto.StatName[s.ID]
		if !ok {
			name = "stat_" + itoa(uint32(s.ID)) // a counter newer than this control plane
		}
		info.Stats[name] = s.Value
	}
	return info
}

// ---- all views ----------------------------------------------------------------

// readPaths are the read-only endpoints, on both the control socket and TCP.
var readPaths = []string{"/summary", "/links", "/routes", "/prefixes", "/tuns", "/dataplane"}

// introspectViews is every read-only view, built together from one dpView.
type introspectViews struct {
	generatedAt time.Time
	byPath      map[string]any // readPaths -> that endpoint's JSON value
}

func (pv *PathVector) buildViews(v dpView) introspectViews {
	at := time.Now()
	links := pv.node.linksFrom(v)
	tuns := tunsFrom(v)
	return introspectViews{
		generatedAt: at,
		byPath: map[string]any{
			"/summary":   pv.summaryFrom(v, links, tuns, at),
			"/links":     links,
			"/routes":    pv.routesFrom(tuns),
			"/prefixes":  pv.prefixesSnapshot(),
			"/tuns":      tuns,
			"/dataplane": dataplaneFrom(v),
		},
	}
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

// registerIntrospection mounts the read-only endpoints on the control API's
// mux: live, read from the data plane on every request. (The TCP listener
// serves snapshots instead, see introspectapi.go.)
func (c *controlAPI) registerIntrospection(mux *http.ServeMux) {
	for _, path := range readPaths {
		mux.HandleFunc(path, getOnly(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, c.pv.buildViews(c.pv.node.readDP(false)).byPath[path])
		}))
	}
	mux.HandleFunc("/", getOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{
			"read": readPaths,
			"write": map[string]string{
				"/advertise": `POST {"prefix": "a.b.c.d/n"}`,
				"/withdraw":  `POST {"prefix": "a.b.c.d/n"}`,
			},
		})
	}))
}
