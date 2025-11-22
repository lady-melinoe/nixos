{ lib, config, ... }:
let
  inherit (lib) mkOption types;
in {
  options.melinoe = {
    nodeId = mkOption {
      type = types.nullOr types.int;
      default = null;
      description = "Unique node ID used for addressing and routing.";
    };

    incusDefaultStorageSource = mkOption {
      type = types.nullOr types.str;
      default = "/var/lib/incus/storage-pools/default";
      description = "Source path for the Incus default storage pool (e.g., /var/lib/incus/storage-pools/default).";
    };

    bgpPeers = mkOption {
      type = types.listOf (types.submodule {
        options = {
          id = mkOption {
            type = types.int;
            description = "Peer node ID (used to derive remote AS as 64512 + id by default).";
          };
          addr = mkOption {
            type = types.str;
            description = "Peer IPv4 address on the underlay (198.19.0.X, but not assumed).";
          };
        };
      });
      default = [ ];
      description = "List of BGP peers with node ID and underlay IPv4 address.";
    };
  };

  config.assertions = [{
    assertion = config.melinoe.nodeId != null;
    message = "melinoe.nodeId must be set for this host.";
  }];
}
