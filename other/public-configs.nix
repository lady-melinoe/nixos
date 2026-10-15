{ lib, melinoeLib, ... }:
let
  inherit (lib) mkOption types;
in
{
  imports = builtins.attrValues (melinoeLib.nodesWith ../nodes "public.nix");

  options.melinoe.other.nodePublicInfo = mkOption {
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
          buildServer = mkOption {
            type = types.nullOr (
              types.submodule {
                options = {
                  hostName = mkOption {
                    type = types.str;
                    description = "SSH hostname other nodes should dial to use this node as a remote builder.";
                  };
                  publicHostCA = mkOption {
                    type = types.nullOr types.str;
                    default = null;
                    description = "The exact @cert-authority line to inject into the consumer's publicHostKey.";
                  };
                  sshUser = mkOption {
                    type = types.str;
                    default = "remotebuild";
                    description = "SSH user other nodes should connect as for remote builds.";
                  };
                  system = mkOption {
                    type = types.str;
                    default = "x86_64-linux";
                    description = "nix.buildMachines system string for this builder.";
                  };
                  supportedFeatures = mkOption {
                    type = types.listOf types.str;
                    default = [ ];
                    description = "nix.buildMachines supportedFeatures for this builder.";
                  };
                };
              }
            );
            default = null;
            description = "Optional remote-build-server details this node publishes, so other nodes can reference it from melinoe.other.distributedBuild.remoteBuildOn by id instead of repeating hostName/CA/system/etc. Leave null for nodes that don't offer remote builds.";
          };
        };
      }
    );
    default = { };
    description = "Per-node public artifacts published by nodes/*/public.nix.";
  };
}
