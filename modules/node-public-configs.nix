{ lib, ... }:
let
  inherit (lib) mkOption types;
  nodesDir = ../nodes;
  entries = builtins.readDir nodesDir;
  publicModules = lib.concatMap (
    name:
    if entries.${name} == "directory" then
      let
        candidate = nodesDir + "/${name}/public.nix";
      in
      if builtins.pathExists candidate then [ candidate ] else [ ]
    else
      [ ]
  ) (builtins.attrNames entries);
in
{
  imports = publicModules;

  options.melinoe.nodePublicInfo = mkOption {
    type = types.attrsOf (
      types.submodule {
        options = {
          wgPubkey = mkOption {
            type = types.str;
            description = "Public WireGuard key for the node.";
          };
          defaultEndpoint = mkOption {
            type = types.nullOr types.str;
            default = null;
            description = "Optional default endpoint hostname/IP published by this node.";
          };
        };
      }
    );
    default = { };
    description = "Per-node public artifacts published by nodes/*/public.nix.";
  };
}
