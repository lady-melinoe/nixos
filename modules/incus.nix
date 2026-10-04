{
  config,
  lib,
  pkgs,
  melinoeLib,
  ...
}:
let
  inherit (lib) mkIf;
  cfg = config.melinoe.node;
in
{
  config = lib.mkMerge [
    {
      programs.bash.shellAliases = {
        icl = "incus cluster list -c nursm";
        ie = "incus exec";
        il = "incus list '-cdevices:uplink.ipv4.address:v4ADDR,nstL,devices:uplink.ipv4.routes:ADDITIONAL ROUTES'";
        imv = "incus move --refresh --stateless";
      };
    }
    (mkIf (!cfg.isVMHost) {
      environment.systemPackages = [ pkgs.incus.client ];
    })
    (mkIf cfg.isVMHost {
      virtualisation.incus.enable = true;
      virtualisation.incus.package = pkgs.incus;
      virtualisation.incus.softDaemonRestart = true;
      systemd.services.incus = melinoeLib.afterMeshAddress;
      melinoe.node.networking.specialHostAccess.tcp = [ 8008 ]; # Incus Cluster
      melinoe.node.networking.openPorts.tcp = [ 8069 ]; # Incus MGMT
      users.users.melinoe.extraGroups = [ "incus-admin" ];
    })
  ];
}
