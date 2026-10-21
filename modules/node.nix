{
  config,
  lib,
  melinoeLib,
  ...
}:
let
  inherit (lib) mkOption types;
  cluster = config.melinoe.cluster;
  self = cluster.nodes.${config.networking.hostName};
in
{
  options.melinoe.node = {
    id = mkOption {
      type = types.int;
      readOnly = true;
      default = self.id;
    };
    address = mkOption {
      type = types.str;
      readOnly = true;
      default = melinoeLib.ip.nodeAddress cluster.networks.nodes self.id;
      description = "This node's own address in melinoe.cluster.networks.nodes.";
    };
  };
}
