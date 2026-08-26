{
  config,
  lib,
  pkgs,
  ...
}:
let
  inherit (lib) mkOption types;
  cfg = config.melinoe;
  routeCfg = config.melinoe.services.melinoe-route;
  rtsCfg = config.melinoe.services.melinoe-rts;
  netCfg = config.melinoe.node.networking;
  nodeID = cfg.node.id;

  # Must match controlSocketPath in melinoe-rts.nix -- only read when
  # rts-integration is on, so melinoe-route never depends on melinoe-rts
  # having actually loaded its option (avoids an option-ordering/module
  # cycle concern) at the cost of the two literals needing to agree.
  rtsControlSocketPath = "/run/melinoe-rts/control.sock";

  mod = a: b: a - (a / b) * b;

  ip4ToInt = ip: lib.foldl' (acc: octet: acc * 256 + lib.toInt octet) 0 (lib.splitString "." ip);

  network24 =
    cidr:
    let
      parts = lib.splitString "/" cidr;
      ipInt = ip4ToInt (lib.elemAt parts 0);
      prefixLength = lib.toInt (lib.elemAt parts 1);
      networkInt = ipInt - (mod ipInt 256);
    in
    assert prefixLength == 24;
    "${toString (networkInt / 16777216)}.${toString (mod (networkInt / 65536) 256)}.${toString (mod (networkInt / 256) 256)}.0";

  routeConfig = pkgs.writeText "melinoe-route.json" (
    builtins.toJSON {
      node_id = nodeID;
      hostname = config.networking.hostName;
      pub_ips = lib.filter (ip: ip != null) (map (entry: entry.pub_ip or null) netCfg.uplinks);
      advertised_routes = routeCfg.extraRoutes;
      regions = cfg.node.regions;
      base_network = network24 cfg.cluster.networking.bgpCidr;
      inner_network = network24 cfg.cluster.networking.hostCidr;
      table_base = netCfg.vmOutboundMarkBase;
      vm_ifaces = map (vm: vm.iface) cfg.cluster.virtualMachines;
      rts_socket_path = lib.optionalString routeCfg.rts-integration rtsControlSocketPath;
    }
  );
  melinoeGoBinary = pkgs.buildGoModule {
    pname = "melinoe-route";
    version = "0.0.8";

    src = ./melinoe-route-daemon-src;
    vendorHash = "sha256-qJtuKtPR43buQk6aSqhgP9FkMJWIvUSWKoab8kn6+Sg=";
  };
in
{
  options.melinoe.services.melinoe-route = {
    enabled = mkOption {
      type = types.bool;
      default = false;
      description = "Enable the Melinoe route daemon.";
    };

    extraRoutes = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "Additional IPv4 prefixes or host routes to advertise via melinoe-route.";
    };

    rts-integration = mkOption {
      type = types.bool;
      default = false;
      description = ''
        Wire this daemon directly into melinoe-rts (the ebpf RTS
        daemon): every node-<id> IPIP tunnel this daemon creates or
        tears down is reported over melinoe-rts's control socket, so
        replies to a flow are forced back out whichever tunnel it first
        arrived on -- instead of melinoe-rts having to guess the RTS set
        from interface-name prefixes on its own.

        Requires both melinoe.services.melinoe-route.enabled and
        melinoe.services.melinoe-rts.enabled to be true.
      '';
    };
  };

  config = {
    assertions = [
      {
        assertion = routeCfg.enabled || routeCfg.extraRoutes == [ ];
        message = "melinoe.services.melinoe-route.extraRoutes requires melinoe.services.melinoe-route.enabled to be true.";
      }
      {
        assertion = !routeCfg.enabled || netCfg.enabled;
        message = "melinoe.node.networking.enabled must be true when melinoe.services.melinoe-route.enabled is true (melinoe.node.networking.uplinks is an uplink property and pub_ips are derived from it).";
      }
      {
        assertion = !routeCfg.rts-integration || (routeCfg.enabled && rtsCfg.enabled);
        message = "melinoe.services.melinoe-route.rts-integration requires both melinoe.services.melinoe-route.enabled and melinoe.services.melinoe-rts.enabled to be true.";
      }
    ];

    melinoe.node.networking.specialHostAccess.tcp = lib.mkIf routeCfg.enabled [ 60198 ]; # melinoe-route daemon port
    melinoe.node.networking.specialLoopbackAccess.ipProtocols = lib.mkIf routeCfg.enabled [ 4 ];

    systemd.services.melinoe-route = lib.mkIf routeCfg.enabled {
      description = "Melinoe route daemon";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ] ++ lib.optional routeCfg.rts-integration "melinoe-rts.service";
      wants = [ "network-online.target" ] ++ lib.optional routeCfg.rts-integration "melinoe-rts.service";
      stopIfChanged = false;
      path = [ ];
      serviceConfig = {
        ExecStart = "${melinoeGoBinary}/bin/melinoe-route --config ${routeConfig}${lib.optionalString (!cfg.node.isVMHost) " --noLocalVMs"}";
        Restart = "always";
        AmbientCapabilities = [ "CAP_NET_ADMIN" ];
        CapabilityBoundingSet = [ "CAP_NET_ADMIN" ];
      };
    };
  };
}
