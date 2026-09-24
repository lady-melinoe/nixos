{
  config,
  lib,
  pkgs,
  melinoeNodeIntraIP,
  ...
}:
let
  inherit (lib) mkOption types;
  cfg = config.melinoe;
  mCfg = cfg.services.melnode;
  netCfg = cfg.node.networking;
  nodeID = cfg.node.id;

  # melnode replaces the old WireGuard + BGP (FRR) + IPIP (melinoe-route)
  # stack. The node's own "identity" is its host-range address, which melnode
  # assigns to every node-<id> tun and always advertises.
  #
  # It is split in two, like an OVS kernel datapath and its userspace daemon:
  #
  #   melnode-dp  the data plane. Owns the UDP socket, the Noise tunnels, the
  #               tun devices and the dst -> next-hop table, and forwards
  #               tunneled IP packets on its own. Everything else it receives
  #               (proto != 0) it hands to melnode-cp. It only does what
  #               melnode-cp tells it to, over dpSocket.
  #   melnode-cp  the control plane. Link liveness, path-vector routing,
  #               tun/route programming, interface addresses, hooks and the
  #               control/introspection APIs. It pushes the configured links
  #               down to melnode-dp when it attaches.
  #
  # melnode-dp keeps forwarding if melnode-cp restarts (a config change that
  # only touches routing/links restarts just the control plane).
  hostAddr = melinoeNodeIntraIP nodeID;

  tunPrefix = "node-";
  controlSocket = "/run/melnode/control.sock";
  dpSocket = "/run/melnode-dp/dp.sock";

  melnodeGoBinary = pkgs.buildGoModule {
    pname = "melnode";
    version = "0.0.2";

    src = ./melinoe-vpn-daemon-src;
    # Builds (and tests) both daemons: bin/melnode-dp and bin/melnode-cp.
    subPackages = [
      "cmd/melnode-dp"
      "cmd/melnode-cp"
    ];
    vendorHash = "sha256-qG9O8ed6bK0WpaQxof+pxsMa7IudmB0rnGTWYjuXm2g=";
  };

  dpBin = "${mCfg.package}/bin/melnode-dp";
  cpBin = "${mCfg.package}/bin/melnode-cp";

  # ---- links ------------------------------------------------------------

  peerIdStr = peer: toString peer.id;

  resolveEndpointHost =
    peer:
    if peer.endpoint != null then
      peer.endpoint
    else
      (cfg.nodePublicInfo.${peerIdStr peer} or { defaultEndpoint = null; }).defaultEndpoint;

  # melnode wants an ip:port literal (no hostnames); IPv6 literals need [].
  formatEndpoint =
    host:
    if lib.hasInfix ":" host then
      "[${host}]:${toString mCfg.port}"
    else
      "${host}:${toString mCfg.port}";

  isIpLiteral =
    host: lib.hasInfix ":" host || builtins.match "[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+" host != null;

  # A peer without an endpoint is listen-only: we never dial it, it must dial
  # us (melnode learns its address from its first authenticated packet).
  mkLink =
    peer:
    let
      host = resolveEndpointHost peer;
    in
    {
      peerid = peer.id;
      peerPubkey = cfg.nodePublicInfo.${peerIdStr peer}.wgPubkey;
      prependCount = peer.prependCount;
    }
    // lib.optionalAttrs (host != null) { endpoint = formatEndpoint host; };

  # ---- helper (hooks + advertiser) ----------------------------------------

  pubIps = lib.filter (ip: ip != null) (map (entry: entry.pub_ip or null) netCfg.uplinks);

  helper = pkgs.writers.writePython3Bin "melnode-helper" {
    # Style linting shouldn't be able to break a deployment build.
    doCheck = false;
  } (builtins.readFile ./melnode-helper-src/melnode-helper.py);

  # Read-only CLI: `mnctl <links|routes> [host[:port]]`.
  mnctl = pkgs.writers.writePython3Bin "mnctl" {
    doCheck = false;
  } (builtins.readFile ./melnode-helper-src/mnctl.py);

  helperConfig = pkgs.writeText "melnode-helper.json" (
    builtins.toJSON {
      node_id = nodeID;
      table_base = netCfg.vmOutboundMarkBase;
      tun_prefix = tunPrefix;
      control_socket = controlSocket;
      pub_ips = pubIps;
      extra_routes = mCfg.extraRoutes;
      vm_ifaces = lib.optionals cfg.node.isVMHost (map (vm: vm.iface) cfg.cluster.virtualMachines);
    }
  );

  helperCmd = "${helper}/bin/melnode-helper --config ${helperConfig}";

  mkHook =
    kind:
    pkgs.writeShellScript "melnode-tun-${kind}-hook" ''
      exec ${helperCmd} hook-${kind} "$@"
    '';

  # ---- melnode configs -----------------------------------------------------

  tomlFormat = pkgs.formats.toml { };

  # Data plane: who it is and how it reaches the network. No links here -- the
  # control plane programs those over dpSocket.
  dpConfig = tomlFormat.generate "melnode-dp.toml" {
    localID = nodeID;
    localPort = mCfg.port;
    tunPrefix = tunPrefix;
    localPrivkeyPath = mCfg.privateKeyFile;
    mtu = mCfg.mtu;
    # Keep melnode's own UDP traffic on the uplink instead of routing it back
    # into the mesh (same job the WireGuard fwMark did).
    fwmark = netCfg.uplinkFwMark;
    socket = dpSocket;
  };

  # Control plane: the links (pushed down to the data plane on attach), path
  # vector, host integration and the APIs.
  cpConfig = tomlFormat.generate "melnode-cp.toml" {
    localID = nodeID;
    dataplaneSocket = dpSocket;
    identityPrefix = "${hostAddr}/32";
    controlSocket = controlSocket;
    # Read-only introspection over TCP (no advertise/withdraw); firewalled to
    # the host range via specialHostAccess below.
    introspectListen = ":${toString mCfg.introspectPort}";
    tunCreateHookBin = "${mkHook "create"}";
    tunDestroyHookBin = "${mkHook "destroy"}";
    link = map mkLink netCfg.peers;
  };

  toolPath = [
    pkgs.iproute2
    pkgs.nftables
  ];
in
{
  options.melinoe.services.melnode = {
    enabled = mkOption {
      type = types.bool;
      default = false;
      description = "Enable the melnode mesh VPN (replaces WireGuard + BGP + IPIP): the melnode-dp data plane and the melnode-cp control plane.";
    };

    port = mkOption {
      type = types.port;
      default = 60198;
      description = ''
        UDP port every node listens on, and every link dials. All nodes use
        the same port; no per-peer ports.
      '';
    };

    introspectPort = mkOption {
      type = types.port;
      default = 60198;
      description = ''
        TCP port for melnode's read-only introspection API (/summary, /links,
        /routes, /prefixes, /tuns; never the write endpoints). Reachable only
        from the host range (melinoe.node.networking.specialHostAccess), so any
        node can view any other node's state.
      '';
    };

    mtu = mkOption {
      type = types.ints.between 576 65000;
      default = 1416;
      description = ''
        MTU of every node-<id> tun. 1416 is WireGuard's 1420 (1500 minus IPv6,
        UDP and WireGuard framing) minus melnode's own 4-byte routing header,
        so a full-size packet still fits a 1500-byte underlay.
      '';
    };

    privateKeyFile = mkOption {
      type = types.str;
      default = "/etc/melinoe/wg.privatekey";
      description = "Path to this node's base64 Curve25519 private key (same format as a WireGuard key).";
    };

    extraRoutes = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "Additional IPv4 prefixes or host routes this node advertises into the mesh.";
    };

    package = mkOption {
      type = types.package;
      default = melnodeGoBinary;
      readOnly = true;
      description = "The built melnode package (bin/melnode-dp and bin/melnode-cp).";
    };
  };

  config = {
    assertions = [
      {
        assertion = mCfg.enabled || mCfg.extraRoutes == [ ];
        message = "melinoe.services.melnode.extraRoutes requires melinoe.services.melnode.enabled to be true.";
      }
      {
        assertion = !mCfg.enabled || netCfg.enabled;
        message = "melinoe.node.networking.enabled must be true when melinoe.services.melnode.enabled is true (melinoe.node.networking.uplinks is an uplink property and pub_ips are derived from it).";
      }
    ]
    ++ lib.optionals mCfg.enabled (
      lib.concatMap (
        peer:
        let
          id = peerIdStr peer;
          info = cfg.nodePublicInfo.${id} or null;
          host = if info == null then null else resolveEndpointHost peer;
        in
        [
          {
            assertion = peer.id != nodeID;
            message = "melinoe.node.networking.peers: node ${id} lists itself as a peer.";
          }
          {
            assertion = info != null;
            message = "melinoe.node.networking.peers: peer ${id} has no melinoe.nodePublicInfo entry (its public key is needed).";
          }
          {
            assertion = host == null || isIpLiteral host;
            message = "melinoe.node.networking.peers: the endpoint for peer ${id} (${toString host}) must be an IP literal - melnode does not resolve hostnames.";
          }
        ]
      ) netCfg.peers
    );

    melinoe.node.networking.openPorts.udp = lib.mkIf mCfg.enabled [ mCfg.port ];
    melinoe.node.networking.specialHostAccess.tcp = lib.mkIf mCfg.enabled [ mCfg.introspectPort ];

    environment.systemPackages = lib.mkIf mCfg.enabled [ mnctl ];

    systemd.services.melnode-dp = lib.mkIf mCfg.enabled {
      description = "melnode data plane (Noise tunnels, tuns, forwarding)";
      wantedBy = [ "multi-user.target" ];
      after = [
        "network-online.target"
        "melinoe-inet-setup.service"
      ];
      wants = [
        "network-online.target"
        "melinoe-inet-setup.service"
      ];
      stopIfChanged = false;
      serviceConfig = {
        ExecStart = "${dpBin} -config ${dpConfig}";
        Restart = "always";
        RestartSec = 1;
        RuntimeDirectory = "melnode-dp";
        AmbientCapabilities = [ "CAP_NET_ADMIN" ];
        CapabilityBoundingSet = [ "CAP_NET_ADMIN" ];
      };
    };

    systemd.services.melnode-cp = lib.mkIf mCfg.enabled {
      description = "melnode control plane (liveness, path-vector routing, host integration)";
      wantedBy = [ "multi-user.target" ];
      after = [
        "melnode-dp.service"
        "network-online.target"
        "melinoe-inet-setup.service"
        "nftables.service"
      ];
      wants = [
        "network-online.target"
        "melinoe-inet-setup.service"
        "nftables.service"
      ];
      # If the data plane is stopped or restarted, so is the control plane
      # (it would just re-attach and re-sync anyway; restarting it is the
      # simplest way to guarantee a clean start against a fresh data plane).
      # The reverse is NOT true: restarting only the control plane leaves the
      # data plane forwarding.
      requires = [ "melnode-dp.service" ];
      stopIfChanged = false;
      # `flush ruleset` on an nftables reload empties melinoe_peer_marks and
      # melinoe_peer_ifaces; restarting the control plane makes it re-adopt the
      # data plane's tuns and re-run the tun create hooks, which repopulate them.
      restartTriggers = [ (builtins.hashString "sha256" config.networking.nftables.ruleset) ];
      # PATH for the tun hooks, which shell out to ip/nft.
      path = toolPath;
      serviceConfig = {
        ExecStart = "${cpBin} -config ${cpConfig}";
        Restart = "always";
        RestartSec = 1;
        RuntimeDirectory = "melnode";
        AmbientCapabilities = [ "CAP_NET_ADMIN" ];
        CapabilityBoundingSet = [ "CAP_NET_ADMIN" ];
      };
    };

    # Advertises this node's prefixes to melnode, and keeps per-tun policy
    # routing / nft pinning in shape. Independent of melnode's lifetime: it
    # notices melnode restarting (new control socket) and re-advertises.
    systemd.services.melnode-helper = lib.mkIf mCfg.enabled {
      description = "melnode helper (route advertisement, per-tun policy routing)";
      wantedBy = [ "multi-user.target" ];
      after = [
        "melnode-cp.service"
        "nftables.service"
      ];
      wants = [ "melnode-cp.service" ];
      stopIfChanged = false;
      path = toolPath;
      serviceConfig = {
        ExecStart = "${helperCmd} daemon";
        Restart = "always";
        RestartSec = 1;
        AmbientCapabilities = [ "CAP_NET_ADMIN" ];
        CapabilityBoundingSet = [ "CAP_NET_ADMIN" ];
      };
    };
  };
}
