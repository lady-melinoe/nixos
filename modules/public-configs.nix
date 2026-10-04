{ lib, melinoeLib, ... }:
let
  inherit (lib) mkOption types;
in
{
  imports = builtins.attrValues (melinoeLib.nodesWith ../nodes "public.nix");

  options.melinoe.nodePublicInfo = mkOption {
    type = types.attrsOf (
      types.submodule {
        options = {
          wgPubkey = mkOption {
            type = types.str;
            description = "Node public key (base64 Curve25519; used as the melnode peer public key, same format as a WireGuard key).";
          };
          defaultEndpoint = mkOption {
            type = types.nullOr types.str;
            default = null;
            description = "Optional default endpoint IP published by this node. Leave null for nodes that only dial out and accept no inbound connections.";
          };
        };
      }
    );
    default = { };
    description = "Per-node public artifacts published by nodes/*/public.nix.";
  };
}
