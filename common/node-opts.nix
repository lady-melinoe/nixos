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
  };

  assertions = [{
    assertion = config.melinoe.nodeId != null;
    message = "melinoe.nodeId must be set for this host.";
  }];
}
