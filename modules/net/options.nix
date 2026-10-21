{ lib, ... }:
let
  inherit (lib) mkOption types;
in
{
  options.melinoe.net = {
    uplinkMark = mkOption {
      type = types.ints.u32;
      default = 51820;
      description = ''
        fwmark that routes return traffic out of the uplinks instead of over the
        mesh, and the id of the table those packets are routed by. Shared by
        uplink.nix, firewall.nix and melnode.nix.
      '';
    };
    outboundMarkBase = mkOption {
      type = types.ints.u32;
      default = 1000;
      description = ''
        Traffic to be routed out via node N carries fwmark and uses table
        outboundMarkBase + N. Shared by firewall.nix and melnode.nix.
      '';
    };
  };
}
