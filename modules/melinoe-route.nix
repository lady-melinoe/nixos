{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.melinoe;
  routeCfg = config.melinoe.node.networking.melinoe-route;
  netCfg = config.melinoe.node.networking;
  nodeID = cfg.node.id;
  # /24-only dotted prefix, e.g. "198.51.100.0/24" -> "198.51.100."
  # This is the addressing scheme the daemon's ipip-tunnel/mesh-address
  # arithmetic is built around (string concatenation of prefix + node id),
  # so it inherently only supports /24 blocks; that part isn't something a
  # flag/config value can generalize away without a bigger rewrite of the
  # addressing logic itself.
  dottedPrefix24 =
    cidr:
    let
      ipPart = lib.head (lib.splitString "/" cidr);
      octets = lib.splitString "." ipPart;
    in
    lib.concatStringsSep "." (lib.take 3 octets) + ".";

  routeConfig = pkgs.writeText "melinoe-route.json" (
    builtins.toJSON {
      node_id = nodeID;
      hostname = config.networking.hostName;
      pub_ips = lib.filter (ip: ip != null) (map (entry: entry.pub_ip or null) netCfg.uplinks);
      advertised_routes = routeCfg.extraRoutes;
      regions = cfg.node.regions;

      # These four used to be hardcoded inside the Go source and were kept
      # in sync only via nix-level assertions. They're now plain config,
      # computed here from the values that are already the source of truth
      # elsewhere in the module tree, so there's nothing left to assert.
      base_prefix = dottedPrefix24 cfg.cluster.networking.bgpCidr;
      inner_prefix = dottedPrefix24 cfg.cluster.networking.hostCidr;
      table_base = netCfg.vmOutboundMarkBase;
      vm_ifaces = map (vm: vm.iface) cfg.cluster.virtualMachines;
    }
  );
  melinoeGoBinary = pkgs.buildGoModule {
    pname = "melinoe-route";
    version = "0.0.6";

    src = ./melinoe-route-daemon-src;
    vendorHash = "sha256-qJtuKtPR43buQk6aSqhgP9FkMJWIvUSWKoab8kn6+Sg=";
  };
in
{
  config = {
    assertions = [
      {
        assertion = routeCfg.enabled || routeCfg.extraRoutes == [ ];
        message = "melinoe.node.networking.melinoe-route.extraRoutes requires melinoe.node.networking.melinoe-route.enabled to be true.";
      }
      {
        assertion = !routeCfg.enabled || netCfg.enabled;
        message = "melinoe.node.networking.enabled must be true when melinoe.node.networking.melinoe-route.enabled is true (melinoe.node.networking.uplinks is an uplink property and pub_ips are derived from it).";
      }
    ];

    melinoe.node.networking.specialHostAccess.tcp = lib.mkIf routeCfg.enabled [ 60198 ];
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
