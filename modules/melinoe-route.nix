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
  netCfg = config.melinoe.node.networking;
  nodeID = cfg.node.id;

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
    }
  );
  melinoeGoBinary = pkgs.buildGoModule {
    pname = "melinoe-route";
    version = "0.0.8";

    src = ./melinoe-route-daemon-src;
    vendorHash = "sha256-4Oo7YKSOeQ2197QhS4jiljtN7OQsBmaNf17lSPg9YKI=";
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
    ];

    melinoe.node.networking.specialHostAccess.tcp = lib.mkIf routeCfg.enabled [ 60198 ]; # melinoe-route daemon port
    melinoe.node.networking.specialLoopbackAccess.ipProtocols = lib.mkIf routeCfg.enabled [ 4 ];

    systemd.services.melinoe-route = lib.mkIf routeCfg.enabled {
      description = "Melinoe route daemon";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
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
