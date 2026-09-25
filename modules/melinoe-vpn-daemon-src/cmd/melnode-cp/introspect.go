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
//	GET /dataplane   the data plane's own counters (drops by cause, punts, injects)
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

func (n *Node) linksSnapshot() []linkInfo {
	byID := map[uint32]dpproto.LinkInfo{}
	cl, release := n.introspectDP()
	defer release()
	if cl != nil {
		if list, err := cl.LinkList(); err == nil {
			for _, li := range list {
				byID[li.PeerID] = li
			}
		}
	}
	out := make([]linkInfo, 0, len(n.links))
	for _, l := range n.links {
		var dp *dpproto.LinkInfo
		if li, ok := byID[l.id]; ok {
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

// tunsSnapshot lists the data plane's tuns with the next hop we want for
// each. Empty while no data plane is attached.
func (r *Router) tunsSnapshot() []tunInfo {
	out := []tunInfo{}
	cl, release := r.node.introspectDP()
	defer release()
	if cl == nil {
		return out
	}
	tuns, err := cl.TunList()
	if err != nil {
		return out
	}
	routes := map[uint32]uint32{}
	if rl, err := cl.RouteList(); err == nil {
		for _, rt := range rl {
			routes[rt.Dst] = rt.NextHop
		}
	}
	for _, t := range tuns {
		ti := tunInfo{PeerID: t.PeerID, Name: t.Name, MTU: int(t.MTU), Started: t.Started}
		if nh, ok := routes[t.PeerID]; ok {
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
	for _, t := range pv.node.router.tunsSnapshot() {
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
	LocalID           uint32  `json:"local_id"`
	PublicKey         string  `json:"public_key"` // empty until the data plane has been attached at least once
	DataplaneAttached bool    `json:"dataplane_attached"`
	UptimeSeconds     float64 `json:"uptime_seconds"`
	MTU               int     `json:"mtu"`
	IdentityPrefix    string  `json:"identity_prefix,omitempty"`
	LinksTotal        int     `json:"links_total"`
	LinksUp           int     `json:"links_up"`
	Destinations      int     `json:"destinations"` // peerids currently reachable
	Tuns              int     `json:"tuns"`
	Prefixes          int     `json:"prefixes"`
}

func (pv *PathVector) summarySnapshot() summaryInfo {
	n := pv.node
	s := summaryInfo{
		LocalID:       n.localID,
		UptimeSeconds: time.Since(processStart).Seconds(),
		MTU:           int(n.mtu.Load()),
	}
	if pk := n.pubkey.Load(); pk != nil {
		s.PublicKey = keyToBase64(pk[:])
	}
	s.DataplaneAttached = n.dp() != nil
	if n.router != nil && n.router.identityPrefix != nil {
		s.IdentityPrefix = n.router.identityPrefix.String()
	}
	links := n.linksSnapshot()
	s.LinksTotal = len(links)
	for _, l := range links {
		if l.State == linkStateUp.String() {
			s.LinksUp++
		}
	}
	s.Tuns = len(n.router.tunsSnapshot())
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

// dataplaneSnapshot dumps the data plane's counters (drops by cause, control
// packets punted/injected). Empty while no data plane is attached.
func (n *Node) dataplaneSnapshot() dataplaneInfo {
	info := dataplaneInfo{Attached: n.dp() != nil}
	cl, release := n.introspectDP()
	defer release()
	if cl == nil {
		return info
	}
	stats, err := cl.Stats()
	if err != nil {
		return info
	}
	info.Stats = make(map[string]uint64, len(stats))
	for _, s := range stats {
		name, ok := dpproto.StatName[s.ID]
		if !ok {
			name = "stat_" + itoa(uint32(s.ID)) // a counter newer than this control plane
		}
		info.Stats[name] = s.Value
	}
	return info
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
	registerReadEndpoints(mux, c.pv, true)
}

// registerReadEndpoints mounts the read-only "show ..." endpoints on mux.
// withWrite only controls whether the index at "/" advertises the
// /advertise and /withdraw endpoints; it does NOT mount them. The TCP
// introspection listener (introspectapi.go) passes false, so it never
// serves anything but GETs of the snapshots below.
func registerReadEndpoints(mux *http.ServeMux, pv *PathVector, withWrite bool) {
	mux.HandleFunc("/summary", getOnly(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, pv.summarySnapshot()) }))
	mux.HandleFunc("/links", getOnly(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, pv.node.linksSnapshot()) }))
	mux.HandleFunc("/routes", getOnly(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, pv.routesSnapshot()) }))
	mux.HandleFunc("/prefixes", getOnly(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, pv.prefixesSnapshot()) }))
	mux.HandleFunc("/dataplane", getOnly(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, pv.node.dataplaneSnapshot()) }))
	mux.HandleFunc("/tuns", getOnly(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, pv.node.router.tunsSnapshot()) }))
	mux.HandleFunc("/", getOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		index := map[string]any{
			"read": []string{"/summary", "/links", "/routes", "/prefixes", "/tuns", "/dataplane"},
		}
		if withWrite {
			index["write"] = map[string]string{
				"/advertise": `POST {"prefix": "a.b.c.d/n"}`,
				"/withdraw":  `POST {"prefix": "a.b.c.d/n"}`,
			}
		}
		writeJSON(w, index)
	}))
}
