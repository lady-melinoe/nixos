{ lib }:
let
  inherit (lib) mkOption types;
in
{
  accessRuleType = description: {
    type = types.submodule {
      options = {
        tcp = mkOption {
          type = types.listOf types.port;
          default = [ ];
          description = "TCP ports to allow.";
        };
        udp = mkOption {
          type = types.listOf types.port;
          default = [ ];
          description = "UDP ports to allow.";
        };
        ipProtocols = mkOption {
          type = types.listOf types.ints.u8;
          default = [ ];
          description = "IP protocol numbers to allow (e.g. 4 for IPIP).";
        };
      };
    };
    default = { };
    inherit description;
  };
}
