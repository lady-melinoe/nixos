{
  config,
  lib,
  pkgs,
  ...
}:
let
  inherit (lib) mkIf;
  cfg = config.melinoe.node;
in
{
  config = mkIf cfg.isVMHost {
    virtualisation.incus.enable = true;
    virtualisation.incus.package = pkgs.incus;
    virtualisation.incus.softDaemonRestart = true;
    melinoe.node.networking.openPorts.tcp = [
      8008 # incus
      8069 # incus
    ];
    users.users.melinoe.extraGroups = [ "incus-admin" ];
    programs.bash.shellAliases = {
      icl = "incus cluster list -c nursm";
      ie = "incus exec";
      il = "incus list '-cdevices:uplink.ipv4.address:v4ADDR,nstL,devices:uplink.ipv4.routes:ADDITIONAL ROUTES'";
    };
  };
}
