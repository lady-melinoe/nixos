package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
)

const (
	TunPrefix    = "node-"
	Port         = "60198" // melinoe-route daemon port
	ProtoMelinoe = 198
	ListVersion  = "melinoe-list 0.0.2"
)

type Config struct {
	NodeID           int      `json:"node_id"`
	Hostname         string   `json:"hostname"`
	PubIPs           []string `json:"pub_ips"`
	AdvertisedRoutes []string `json:"advertised_routes"`
	Regions          []string `json:"regions"`
	BaseNetwork      string   `json:"base_network"`
	InnerNetwork     string   `json:"inner_network"`
	TableBase        int      `json:"table_base"`
	VMIfaces         []string `json:"vm_ifaces"`
	// RTSSocketPath is the melinoe-rts control socket (see the ebpf
	// project's --control-socket) to notify as tunnels come and go.
	// Empty disables notification entirely -- ensureTunnel/flushTunnel
	// just skip it.
	RTSSocketPath string `json:"rts_socket_path"`
}

type network24 [4]byte

func parseNetwork24(s string) (network24, bool) {
	ip := net.ParseIP(s)
	if ip == nil {
		return network24{}, false
	}
	ip4 := ip.To4()
	if ip4 == nil || ip4[3] != 0 {
		return network24{}, false
	}
	return network24{ip4[0], ip4[1], ip4[2], ip4[3]}, true
}

func ones(n *net.IPNet) int {
	bits, _ := n.Mask.Size()
	return bits
}

func (n network24) host(id int) string {
	return fmt.Sprintf("%d.%d.%d.%d", n[0], n[1], n[2], id)
}

func (n network24) hostID(ip net.IP) (int, bool) {
	ip4 := ip.To4()
	if ip4 == nil || ip4[0] != n[0] || ip4[1] != n[1] || ip4[2] != n[2] {
		return 0, false
	}
	return int(ip4[3]), true
}

type PeerState struct {
	RemoteInner     string
	Tun             string
	TunnelPeerRoute string
}

type RouteMap map[string]bool

// ListResponse is the /list wire format: what a node reports about itself
// and the routes it wants peers to carry.
type ListResponse struct {
	Version  string   `json:"version"`
	NodeID   int      `json:"node_id"`
	Hostname string   `json:"hostname"`
	Regions  []string `json:"regions"`
	Routes   []string `json:"routes"`
}

type Engine struct {
	nodeID           int
	hostname         string
	pubIPs           []string
	advertisedRoutes []string
	localRegions     []string
	hostAddr         string
	noLocalVMs       bool

	baseNet   network24
	innerNet  network24
	tableBase int
	vmIfaces  map[string]bool
	rtsSocket string

	peerMu      sync.RWMutex
	peerRegions map[int][]string
}

func NewEngine(cfg Config, noLocalVMs bool) *Engine {
	baseNet, ok := parseNetwork24(cfg.BaseNetwork)
	if !ok {
		log.Fatal("critical: base_network missing or invalid in configuration (see melinoe.cluster.networking.bgpCidr)")
	}
	innerNet, ok := parseNetwork24(cfg.InnerNetwork)
	if !ok {
		log.Fatal("critical: inner_network missing or invalid in configuration (see melinoe.cluster.networking.hostCidr)")
	}
	if cfg.TableBase == 0 {
		log.Fatal("critical: table_base missing from configuration (see melinoe.node.networking.vmOutboundMarkBase)")
	}
	if cfg.NodeID < 0 || cfg.NodeID > 255 {
		log.Fatalf("critical: node_id %d out of range for a /24 network (0-255)", cfg.NodeID)
	}

	vmIfaces := make(map[string]bool, len(cfg.VMIfaces))
	for _, iface := range cfg.VMIfaces {
		vmIfaces[iface] = true
	}

	return &Engine{
		nodeID:           cfg.NodeID,
		hostname:         cfg.Hostname,
		pubIPs:           cfg.PubIPs,
		advertisedRoutes: cfg.AdvertisedRoutes,
		localRegions:     cfg.Regions,
		hostAddr:         innerNet.host(cfg.NodeID),
		noLocalVMs:       noLocalVMs,
		baseNet:          baseNet,
		innerNet:         innerNet,
		tableBase:        cfg.TableBase,
		vmIfaces:         vmIfaces,
		rtsSocket:        cfg.RTSSocketPath,
		peerRegions:      make(map[int][]string),
	}
}

func (e *Engine) normalizePrefix(val string) string {
	val = strings.TrimSpace(val)
	if val == "" {
		return ""
	}
	if !strings.Contains(val, "/") {
		ip := net.ParseIP(val)
		if ip == nil {
			return ""
		}
		if ip.To4() != nil {
			return val + "/32"
		}
		return val + "/128"
	}
	_, ipNet, err := net.ParseCIDR(val)
	if err != nil {
		return ""
	}
	return ipNet.String()
}

func (e *Engine) addRoute(routes RouteMap, val string) {
	if norm := e.normalizePrefix(val); norm != "" {
		routes[norm] = true
	}
}

func (e *Engine) formatRegions(nodeID int) []string {
	if nodeID == e.nodeID {
		return e.localRegions
	}
	e.peerMu.RLock()
	defer e.peerMu.RUnlock()
	return e.peerRegions[nodeID]
}

func (e *Engine) getLocalRouteSet(ctx context.Context) RouteMap {
	routes := make(RouteMap)

	if !e.noLocalVMs {
		nlRoutes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
		if err == nil {
			for _, r := range nlRoutes {
				if r.LinkIndex <= 0 || r.Dst == nil {
					continue
				}
				link, err := netlink.LinkByIndex(r.LinkIndex)
				if err == nil && e.vmIfaces[link.Attrs().Name] {
					e.addRoute(routes, r.Dst.String())
				}
			}
		}
	}

	for _, ip := range e.pubIPs {
		e.addRoute(routes, ip)
	}
	for _, r := range e.advertisedRoutes {
		e.addRoute(routes, r)
	}
	return routes
}

func (e *Engine) serializeRouteList(routes RouteMap) []byte {
	var sortedRoutes []string
	for r := range routes {
		sortedRoutes = append(sortedRoutes, r)
	}
	sort.Strings(sortedRoutes)

	resp := ListResponse{
		Version:  ListVersion,
		NodeID:   e.nodeID,
		Hostname: e.hostname,
		Regions:  e.formatRegions(e.nodeID),
		Routes:   sortedRoutes,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return nil
	}
	return b
}

func (e *Engine) fetchRemoteRouteList(ctx context.Context, remoteInner string) []string {
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, fmt.Sprintf("http://%s:%s/list", remoteInner, Port), nil)
	if err != nil {
		return nil
	}

	remoteResp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer remoteResp.Body.Close()

	var resp ListResponse
	if err := json.NewDecoder(remoteResp.Body).Decode(&resp); err != nil {
		return nil
	}

	e.peerMu.Lock()
	e.peerRegions[resp.NodeID] = resp.Regions
	e.peerMu.Unlock()

	routesMap := make(RouteMap)
	for _, r := range resp.Routes {
		e.addRoute(routesMap, r)
	}

	var routes []string
	for r := range routesMap {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	return routes
}

func (e *Engine) getCurrentRoutesForTunnel(ctx context.Context, tun string) []netlink.Route {
	link, err := netlink.LinkByName(tun)
	if err != nil {
		return nil
	}

	nlRoutes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil
	}

	var routes []netlink.Route
	for _, r := range nlRoutes {
		if r.LinkIndex == link.Attrs().Index && r.Protocol == ProtoMelinoe && r.Dst != nil {
			routes = append(routes, r)
		}
	}
	return routes
}

func (e *Engine) existingMelinoeTunnels(ctx context.Context) []string {
	links, err := netlink.LinkList()
	if err != nil {
		return nil
	}
	var tunnels []string
	for _, link := range links {
		if link.Type() == "ipip" {
			name := link.Attrs().Name
			if strings.HasPrefix(name, TunPrefix) {
				tunnels = append(tunnels, name)
			}
		}
	}
	return tunnels
}

func tunnelNodeID(tun string) (int, bool) {
	id, err := strconv.Atoi(strings.TrimPrefix(tun, TunPrefix))
	if err != nil || id < 0 || id > 255 {
		return 0, false
	}
	return id, true
}

// notifyRTS tells the melinoe-rts (ebpf) daemon, over its control
// socket, that tun should (op="add") or should no longer (op="remove")
// have its replies forced back out the interface they arrived on. Best
// effort and fire-and-forget: a dial/write failure (rts daemon not
// running yet, socket not there this boot, etc.) is logged and dropped
// rather than retried here, because deployOnce re-derives and re-sends
// the full desired state every 2 seconds anyway (see ensureTunnel's call
// site) -- a missed "add" self-heals on the next tick, and a missed
// "remove" self-heals as soon as the ebpf side's own GC or interface-
// detach handling catches up.
func (e *Engine) notifyRTS(op, tun string) {
	if e.rtsSocket == "" {
		return
	}
	conn, err := net.DialTimeout("unix", e.rtsSocket, 500*time.Millisecond)
	if err != nil {
		log.Printf("rts notify: dial %s: %v", e.rtsSocket, err)
		return
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(struct {
		Op    string `json:"op"`
		Iface string `json:"iface"`
	}{Op: op, Iface: tun}); err != nil {
		log.Printf("rts notify: encode %s %s: %v", op, tun, err)
	}
}

func (e *Engine) flushTunnel(ctx context.Context, tun string) {
	remoteID, ok := tunnelNodeID(tun)
	if !ok {
		return
	}
	table := e.tableBase + remoteID

	nlRoutes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err == nil {
		for _, r := range nlRoutes {
			if r.Table == table {
				_ = netlink.RouteDel(&r)
			}
		}
	}

	rule := netlink.NewRule()
	rule.Mark = table
	rule.Table = table
	_ = netlink.RuleDel(rule)

	e.notifyRTS("remove", tun)

	if link, err := netlink.LinkByName(tun); err == nil {
		_ = netlink.LinkSetDown(link)
		if err := netlink.LinkDel(link); err != nil {
			log.Printf("error: failed to delete tunnel device %s via netlink: %v", tun, err)
		}
	}
}

func (e *Engine) buildInterfaceCache(ctx context.Context) map[string]bool {
	cache := make(map[string]bool)
	links, err := netlink.LinkList()
	if err != nil {
		return cache
	}
	for _, link := range links {
		cache[link.Attrs().Name] = true
	}
	return cache
}

func (e *Engine) ensureTunnel(ctx context.Context, linkCache map[string]bool, localVIP, localInner, remoteVIP, remoteInner, tun string, table int) bool {
	var link netlink.Link
	var err error

	localIPAddr := net.ParseIP(localVIP)
	remoteIPAddr := net.ParseIP(remoteVIP)

	iptun := &netlink.Iptun{
		LinkAttrs: netlink.LinkAttrs{
			Name: tun,
		},
		Local:  localIPAddr,
		Remote: remoteIPAddr,
		Ttl:    64,
	}

	if !linkCache[tun] {
		log.Printf("instantiating virtual tunnel link interface: %s", tun)
		if err := netlink.LinkAdd(iptun); err != nil {
			log.Printf("critical: netlink allocation error for interface %s: %v", tun, err)
			return false
		}

		link = iptun
	} else {
		link, err = netlink.LinkByName(tun)
		if err != nil {
			return false
		}

		if existingTun, ok := link.(*netlink.Iptun); !ok || !existingTun.Local.Equal(localIPAddr) || !existingTun.Remote.Equal(remoteIPAddr) || existingTun.Ttl != 64 {
			_ = netlink.LinkDel(link)
			if err := netlink.LinkAdd(iptun); err != nil {
				log.Printf("critical: netlink reallocation error for interface %s: %v", tun, err)
				return false
			}
			link = iptun
		}
	}

	localIP, localNet, _ := net.ParseCIDR(localInner + "/32")
	localNet.IP = net.ParseIP(remoteInner)
	addr := &netlink.Addr{
		IPNet: &net.IPNet{IP: localIP, Mask: localNet.Mask},
		Peer:  localNet,
	}
	_ = netlink.AddrReplace(link, addr)

	_ = netlink.LinkSetMTU(link, 1400)
	if err := netlink.LinkSetUp(link); err != nil {
		log.Printf("error: link activation failed for device %s: %v", tun, err)
	}

	rule := netlink.NewRule()
	rule.Mark = table
	rule.Table = table
	_ = netlink.RuleDel(rule)
	if err := netlink.RuleAdd(rule); err != nil {
		log.Printf("error: policy routing rule assignment failed for table %d: %v", table, err)
	}

	_, defaultNet, _ := net.ParseCIDR("0.0.0.0/0")
	dr := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       defaultNet,
		Table:     table,
		Scope:     netlink.SCOPE_UNIVERSE,
	}
	_ = netlink.RouteReplace(dr)

	return true
}

func (e *Engine) regionPriority(remoteID int) (bool, bool, bool, int) {
	e.peerMu.RLock()
	remoteRegs := e.peerRegions[remoteID]
	e.peerMu.RUnlock()

	same := func(idx int) bool {
		return len(e.localRegions) > idx && len(remoteRegs) > idx && e.localRegions[idx] == remoteRegs[idx]
	}
	return !same(2), !same(1), !same(0), remoteID
}

func (e *Engine) getBGPRouteTablePeerIDs(ctx context.Context) []int {
	nlRoutes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil
	}

	remoteIDsMap := make(map[int]bool)
	for _, r := range nlRoutes {
		if r.Protocol == 186 && r.Dst != nil && ones(r.Dst) == 32 {
			if hostID, ok := e.baseNet.hostID(r.Dst.IP); ok && hostID != e.nodeID {
				remoteIDsMap[hostID] = true
			}
		}
	}

	var remoteIDs []int
	for id := range remoteIDsMap {
		remoteIDs = append(remoteIDs, id)
	}

	sort.Slice(remoteIDs, func(i, j int) bool {
		notSame2I, notSame1I, notSame0I, idI := e.regionPriority(remoteIDs[i])
		notSame2J, notSame1J, notSame0J, idJ := e.regionPriority(remoteIDs[j])
		if notSame2I != notSame2J {
			return notSame2I
		}
		if notSame1I != notSame1J {
			return notSame1I
		}
		if notSame0I != notSame0J {
			return notSame0I
		}
		return idI < idJ
	})
	return remoteIDs
}

func (e *Engine) computeDesiredRoutes(peerStates []PeerState, fetchedRoutes [][]string, localInnerRoute string, locallyAdvertised RouteMap) map[string]string {
	desired := make(map[string]string)
	for i, state := range peerStates {
		for _, prefix := range fetchedRoutes[i] {
			if prefix == localInnerRoute || prefix == state.TunnelPeerRoute || locallyAdvertised[prefix] {
				continue
			}
			if _, exists := desired[prefix]; !exists {
				desired[prefix] = state.Tun
			}
		}
	}
	return desired
}

func (e *Engine) reconcileAllRoutes(ctx context.Context, peerStates []PeerState, desiredRoutes map[string]string) {
	currentRoutes := make(map[string]string)
	currentRouteObjs := make(map[string]netlink.Route)
	tunnelPeerRoutes := make(RouteMap)
	for _, state := range peerStates {
		tunnelPeerRoutes[state.TunnelPeerRoute] = true
	}
	for _, state := range peerStates {
		for _, r := range e.getCurrentRoutesForTunnel(ctx, state.Tun) {
			prefix := r.Dst.String()
			if !tunnelPeerRoutes[prefix] {
				currentRoutes[prefix] = state.Tun
				currentRouteObjs[prefix] = r
			}
		}
	}
	for prefix, tun := range desiredRoutes {
		if currentRoutes[prefix] != tun {
			log.Printf("mutating routing table: adding network target destination prefix %s via %s", prefix, tun)
			link, err := netlink.LinkByName(tun)
			if err == nil {
				_, dstNet, _ := net.ParseCIDR(prefix)
				route := &netlink.Route{
					LinkIndex: link.Attrs().Index,
					Dst:       dstNet,
					Priority:  1,
					Protocol:  ProtoMelinoe,
					Scope:     netlink.SCOPE_LINK,
				}
				if err := netlink.RouteReplace(route); err != nil {
					log.Printf("error: kernel netlink route injection failure: prefix %s via %s: %v", prefix, tun, err)
				}
			}
		}
	}
	for prefix, tun := range currentRoutes {
		if desiredRoutes[prefix] != tun {
			log.Printf("mutating routing table: evicting stale network target prefix %s from device %s", prefix, tun)
			r := currentRouteObjs[prefix]

			rDel := netlink.Route{
				LinkIndex: r.LinkIndex,
				Dst:       r.Dst,
				Protocol:  r.Protocol,
				Table:     r.Table,
				Scope:     r.Scope,
				Priority:  r.Priority,
			}
			if err := netlink.RouteDel(&rDel); err != nil {
				log.Printf("error: kernel netlink route destruction failure: prefix %s from %s: %v", prefix, tun, err)
			}
		}
	}
}

func (e *Engine) deployOnce(ctx context.Context) {
	localVIP := e.baseNet.host(e.nodeID)
	localInner := e.innerNet.host(e.nodeID)
	localInnerRoute := localInner + "/32"
	localVIPRoute := localVIP + "/32"

	if lo, err := netlink.LinkByName("lo"); err == nil {
		ip, ipNet, _ := net.ParseCIDR(localVIPRoute)
		_ = netlink.AddrReplace(lo, &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: ipNet.Mask}})
	}

	linkCache := e.buildInterfaceCache(ctx)
	locallyAdvertised := e.getLocalRouteSet(ctx)
	remoteNodeIDs := e.getBGPRouteTablePeerIDs(ctx)
	remoteIDsMap := make(map[int]bool)
	for _, id := range remoteNodeIDs {
		remoteIDsMap[id] = true
	}
	for _, tun := range e.existingMelinoeTunnels(ctx) {
		if remoteID, ok := tunnelNodeID(tun); !ok || !remoteIDsMap[remoteID] {
			e.flushTunnel(ctx, tun)
		}
	}
	var peerStates []PeerState
	for _, remoteID := range remoteNodeIDs {
		remoteVIP := e.baseNet.host(remoteID)
		remoteInner := e.innerNet.host(remoteID)
		tun := TunPrefix + strconv.Itoa(remoteID)
		table := e.tableBase + remoteID
		if !e.ensureTunnel(ctx, linkCache, localVIP, localInner, remoteVIP, remoteInner, tun, table) {
			continue
		}
		e.notifyRTS("add", tun)
		peerStates = append(peerStates, PeerState{
			RemoteInner:     remoteInner,
			Tun:             tun,
			TunnelPeerRoute: remoteInner + "/32",
		})
	}
	if len(peerStates) == 0 {
		return
	}
	fetchedRoutes := make([][]string, len(peerStates))
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 32)
	for i, state := range peerStates {
		wg.Add(1)
		semaphore <- struct{}{}
		go func(idx int, ps PeerState) {
			defer wg.Done()
			defer func() { <-semaphore }()
			fetchedRoutes[idx] = e.fetchRemoteRouteList(ctx, ps.RemoteInner)
		}(i, state)
	}
	wg.Wait()
	desiredRoutes := e.computeDesiredRoutes(peerStates, fetchedRoutes, localInnerRoute, locallyAdvertised)
	e.reconcileAllRoutes(ctx, peerStates, desiredRoutes)
}

func (e *Engine) Start(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("Stopping orchestration framework control loop tick sequence gracefully...")
			return
		case <-ticker.C:
			runCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
			e.deployOnce(runCtx)
			cancel()
		}
	}
}

func main() {
	var configPath string
	var noLocalVMs bool
	flag.StringVar(&configPath, "config", "", "Path to configuration json file")
	flag.BoolVar(&noLocalVMs, "noLocalVMs", false, "Skip scanning vm_ifaces-listed interfaces for locally-advertised routes; use on nodes that never run local containers/VMs")
	flag.Parse()

	if configPath == "" {
		log.Fatal("critical: initialization parameter missing: --config required")
	}

	b, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("critical: failed reading configuration target: %v", err)
	}

	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Fatalf("critical: configuration parse exception: %v", err)
	}

	engine := NewEngine(cfg, noLocalVMs)

	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	mux := http.NewServeMux()
	mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		routes := engine.getLocalRouteSet(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(engine.serializeRouteList(routes))
	})

	server := &http.Server{
		Addr:    net.JoinHostPort(engine.hostAddr, Port),
		Handler: mux,
	}

	go func() {
		log.Printf("Starting web routing listener server on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("critical: web routing framework listener failed: %v", err)
		}
	}()

	shutdownDone := make(chan struct{})

	go func() {
		sig := <-sigChan
		log.Printf("Termination request signal captured (%s). Executing defensive control shutdown...", sig)

		rootCancel()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("warning: web routing stack failed to yield active tasks cleanly during shutdown: %v", err)
		}
		close(shutdownDone)
	}()

	log.Println("Starting Melinoe Deployment Engine...")
	engine.Start(rootCtx)

	<-shutdownDone
	log.Println("Melinoe Route Engine shutdown procedure completed successfully.")
}
