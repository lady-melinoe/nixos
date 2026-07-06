{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.melinoe;
  nodeID = cfg.nodeId;

  routeConfig = pkgs.writeText "melinoe-route.json" (
    builtins.toJSON {
      node_id = nodeID;
      hostname = config.networking.hostName;
      pub_ips = lib.filter (ip: ip != null) (map (entry: entry.pub_ip or null) cfg.internet);
      advertised_routes = cfg.advertisedRoutes;
      regions = cfg.regions;
    }
  );

  melinoeGoBinary = pkgs.buildGoModule {
    pname = "melinoe-route";
    version = "0.0.6";

    src = pkgs.runCommand "melinoe-route-src" { } ''
      mkdir -p $out

      cat << 'EOF' > $out/go.mod
      module melinoe-route

      go 1.21

      require (
          github.com/vishvananda/netlink v1.1.0
          github.com/vishvananda/netns v0.0.4
      )

      require golang.org/x/sys v0.13.0 // indirect
      EOF

      cat << 'EOF' > $out/go.sum
      github.com/vishvananda/netlink v1.1.0 h1:1iyaYNBLmP6L0220aDnYQpo1QEV4t4hJ+xEEhhJH8j0=
      github.com/vishvananda/netlink v1.1.0/go.mod h1:cTgwzPIzzgDAYoQrMm0EdrjRUBkTqKYppBueQtXaqoE=
      github.com/vishvananda/netns v0.0.4 h1:Oeaw1EM2JMxD51g9uhtC0D7erkIjgmj8+JZc26m1YX8=
      github.com/vishvananda/netns v0.0.4/go.mod h1:SpkAiCQRtJ6TvvxPnOSyH3BMl6unz3xZlaprSwhNNJM=
      golang.org/x/sys v0.13.0 h1:Af8nKPmuFypiUBjVoU9V20FiaFXOcuZI21p0ycVYYGE=
      golang.org/x/sys v0.13.0/go.mod h1:oPkhp1MJrh7nUepCBck5+mAzfO9JrbApNNgaTdGDITg=
      EOF

      cat << 'EOF' > $out/main.go
      package main

      import (
          "context"
          "encoding/json"
          "flag"
          "fmt"
          "io"
          "log"
          "net"
          "net/http"
          "os"
          "os/signal"
          "regexp"
          "sort"
          "strconv"
          "strings"
          "sync"
          "syscall"
          "time"

          "github.com/vishvananda/netlink"
      )

      const (
          BasePrefix   = "198.51.100."
          InnerPrefix  = "198.18.0."
          TunPrefix    = "node-"
          Port         = "60198"
          ProtoMelinoe = 198
      )

      type Config struct {
          NodeID           interface{} `json:"node_id"`
          Hostname         string      `json:"hostname"`
          PubIPs           []string    `json:"pub_ips"`
          AdvertisedRoutes []string    `json:"advertised_routes"`
          Regions          []string    `json:"regions"`
      }

      type PeerState struct {
          RemoteInner     string
          Tun             string
          TunnelPeerRoute string
      }

      type RouteMap map[string]bool

      type Engine struct {
          nodeID           string
          hostname         string
          pubIPs           []string
          advertisedRoutes []string
          localRegions     []string
          hostAddr         string

          peerMu      sync.RWMutex
          peerRegions map[string][]string

          nodeIDRegex *regexp.Regexp
          headerRegex *regexp.Regexp
          bgpRegex    *regexp.Regexp
      }

      func NewEngine(cfg Config) *Engine {
          var id string
          switch v := cfg.NodeID.(type) {
          case string:
              id = v
          case float64:
              id = strconv.Itoa(int(v))
          default:
              log.Fatal("critical: unexpected or missing json type for node_id")
          }

          return &Engine{
              nodeID:           id,
              hostname:         cfg.Hostname,
              pubIPs:           cfg.PubIPs,
              advertisedRoutes: cfg.AdvertisedRoutes,
              localRegions:     cfg.Regions,
              hostAddr:         InnerPrefix + id,
              peerRegions:      make(map[string][]string),
              nodeIDRegex:      regexp.MustCompile(`^[0-9]+$`),
              headerRegex:      regexp.MustCompile(`^(\d+):\s*.*?\-\s*(.*)$`),
              bgpRegex:         regexp.MustCompile(`^198\.51\.100\.([0-9]+)(?:/32)?$`),
          }
      }

      func (e *Engine) isValidNodeID(val string) bool {
          return e.nodeIDRegex.MatchString(val)
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

      func (e *Engine) formatRegion(nodeID string) string {
          if nodeID == e.nodeID {
              return strings.Join(e.localRegions, " ")
          }
          e.peerMu.RLock()
          defer e.peerMu.RUnlock()
          return strings.Join(e.peerRegions[nodeID], " ")
      }

      func (e *Engine) getLocalRouteSet(ctx context.Context) RouteMap {
          routes := make(RouteMap)

          nlRoutes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
          if err == nil {
              for _, r := range nlRoutes {
                  if r.LinkIndex <= 0 || r.Dst == nil {
                      continue
                  }
                  link, err := netlink.LinkByIndex(r.LinkIndex)
                  if err == nil && strings.HasPrefix(link.Attrs().Name, "vm-") {
                      e.addRoute(routes, r.Dst.String())
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

      func (e *Engine) serializeRouteList(routes RouteMap) string {
          var sortedRoutes []string
          for r := range routes {
              sortedRoutes = append(sortedRoutes, r)
          }
          sort.Strings(sortedRoutes)

          var sb strings.Builder
          sb.WriteString("// melinoe-list 0.0.1\n")
          fmt.Fprintf(&sb, "// %s: %s - %s\n", e.nodeID, e.hostname, e.formatRegion(e.nodeID))
          for _, r := range sortedRoutes {
              sb.WriteString(r + "\n")
          }
          return sb.String()
      }

      func (e *Engine) parseRemoteHeader(body string) {
          lines := strings.Split(body, "\n")
          for _, line := range lines {
              if !strings.HasPrefix(line, "//") {
                  continue
              }
              content := strings.TrimSpace(line[2:])
              matches := e.headerRegex.FindStringSubmatch(content)
              if matches == nil {
                  continue
              }
              nodeID := matches[1]
              regionsStr := strings.TrimSpace(matches[2])

              e.peerMu.Lock()
              if regionsStr == "" {
                  e.peerRegions[nodeID] = []string{}
              } else {
                  e.peerRegions[nodeID] = strings.Fields(regionsStr)
              }
              e.peerMu.Unlock()
          }
      }

      func (e *Engine) fetchRemoteRouteList(ctx context.Context, remoteInner string) []string {
          reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
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

          b, err := io.ReadAll(remoteResp.Body)
          if err != nil {
              return nil
          }
          body := string(b)

          e.parseRemoteHeader(body)

          routesMap := make(RouteMap)
          lines := strings.Split(body, "\n")
          for _, line := range lines {
              if idx := strings.Index(line, "//"); idx != -1 {
                  line = line[:idx]
              }
              line = strings.TrimSpace(line)
              if line == "" {
                  continue
              }
              e.addRoute(routesMap, line)
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

      func (e *Engine) flushTunnel(ctx context.Context, tun string) {
          remoteID := strings.TrimPrefix(tun, TunPrefix)
          if !e.isValidNodeID(remoteID) {
              return
          }
          idInt, _ := strconv.Atoi(remoteID)
          table := 1000 + idInt

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

      func (e *Engine) ensureTunnel(ctx context.Context, linkCache map[string]bool, remoteID, localVIP, localInner, remoteVIP, remoteInner, tun, tableStr string) bool {
          table, _ := strconv.Atoi(tableStr)
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

      func (e *Engine) regionPriority(remoteID string) (bool, bool, bool, int) {
          e.peerMu.RLock()
          remoteRegs := e.peerRegions[remoteID]
          e.peerMu.RUnlock()

          same := func(idx int) bool {
              return len(e.localRegions) > idx && len(remoteRegs) > idx && e.localRegions[idx] == remoteRegs[idx]
          }
          idInt, _ := strconv.Atoi(remoteID)
          return !same(2), !same(1), !same(0), idInt
      }

      func (e *Engine) getBGPRouteTablePeerIDs(ctx context.Context) []string {
          nlRoutes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
          if err != nil {
              return nil
          }

          remoteIDsMap := make(RouteMap)
          for _, r := range nlRoutes {
              if r.Protocol == 186 && r.Dst != nil {
                  matches := e.bgpRegex.FindStringSubmatch(r.Dst.String())
                  if matches != nil {
                      remoteID := matches[1]
                      if remoteID != e.nodeID {
                          remoteIDsMap[remoteID] = true
                      }
                  }
              }
          }

          var remoteIDs []string
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
                  
                  // Strip the route down to strict matching lookup parameters to prevent ESRCH errors
                  rDel := netlink.Route{
                      LinkIndex: r.LinkIndex,
                      Dst:       r.Dst,
                      Protocol:  r.Protocol,
                      Table:     r.Table,
                  }
                  if err := netlink.RouteDel(&rDel); err != nil {
                      log.Printf("error: kernel netlink route destruction failure: prefix %s from %s: %v", prefix, tun, err)
                  }
              }
          }
      }

      func (e *Engine) deployOnce(ctx context.Context) {
          if !e.isValidNodeID(e.nodeID) {
              log.Printf("error: invalid local node id: %s", e.nodeID)
              return
          }
          localVIP := BasePrefix + e.nodeID
          localInner := InnerPrefix + e.nodeID
          localInnerRoute := localInner + "/32"
          localVIPRoute := localVIP + "/32"

          if lo, err := netlink.LinkByName("lo"); err == nil {
              ip, ipNet, _ := net.ParseCIDR(localVIPRoute)
              _ = netlink.AddrReplace(lo, &netlink.Addr{IPNet: &net.IPNet{IP: ip, Mask: ipNet.Mask}})
          }

          linkCache := e.buildInterfaceCache(ctx)
          locallyAdvertised := e.getLocalRouteSet(ctx)
          remoteNodeIDs := e.getBGPRouteTablePeerIDs(ctx)
          remoteIDsMap := make(RouteMap)
          for _, id := range remoteNodeIDs {
              remoteIDsMap[id] = true
          }
          for _, tun := range e.existingMelinoeTunnels(ctx) {
              if remoteID := strings.TrimPrefix(tun, TunPrefix); !remoteIDsMap[remoteID] {
                  e.flushTunnel(ctx, tun)
              }
          }
          var peerStates []PeerState
          for _, remoteID := range remoteNodeIDs {
              if !e.isValidNodeID(remoteID) {
                  continue
              }
              remoteVIP := BasePrefix + remoteID
              remoteInner := InnerPrefix + remoteID
              tun := TunPrefix + remoteID
              idInt, _ := strconv.Atoi(remoteID)
              table := strconv.Itoa(1000 + idInt)
              if !e.ensureTunnel(ctx, linkCache, remoteID, localVIP, localInner, remoteVIP, remoteInner, tun, table) {
                  continue
              }
              peerStates = append(peerStates, PeerState{
                  RemoteInner: remoteInner,
                  Tun:         tun,
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
          ticker := time.NewTicker(3 * time.Second)
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
          flag.StringVar(&configPath, "config", "", "Path to configuration json file")
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

          engine := NewEngine(cfg)

          rootCtx, rootCancel := context.WithCancel(context.Background())
          defer rootCancel()

          sigChan := make(chan os.Signal, 1)
          signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

          mux := http.NewServeMux()
          mux.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
              routes := engine.getLocalRouteSet(r.Context())
              w.Header().Set("Content-Type", "text/plain; charset=utf-8")
              w.WriteHeader(http.StatusOK)
              io.WriteString(w, engine.serializeRouteList(routes))
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
      EOF
    '';
    vendorHash = "sha256-qJtuKtPR43buQk6aSqhgP9FkMJWIvUSWKoab8kn6+Sg=";
  };
in
{
  systemd.services.melinoe-route = {
    description = "Melinoe route daemon (Production Optimized)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network-online.target" ];
    wants = [ "network-online.target" ];

    path = [ ];
    serviceConfig = {
      ExecStart = "${melinoeGoBinary}/bin/melinoe-route --config ${routeConfig}";
      Restart = "always";
      AmbientCapabilities = [ "CAP_NET_ADMIN" ];
      CapabilityBoundingSet = [ "CAP_NET_ADMIN" ];
    };
  };
}
