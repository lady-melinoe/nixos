{
  config,
  lib,
  pkgs,
  melinoeLib,
  ...
}:
let
  cfg = config.melinoe.incus;
in
{
  options.melinoe.incus.enable = lib.mkOption {
    type = lib.types.bool;
    default = true;
    description = "Run Incus and host containers and VMs. Without it the node still joins the mesh and routes to everyone else's.";
  };

  config = lib.mkMerge [
    {
      programs.bash.shellAliases = {
        icl = "incus cluster list -c nursm";
        ie = "incus exec";
        il = "incus list '-cdevices:uplink.ipv4.address:v4ADDR,nstL,devices:uplink.ipv4.routes:ADDITIONAL ROUTES'";
        imv = "incus move --refresh --stateless";
      };
    }

    (lib.mkIf (!cfg.enable) {
      environment.systemPackages = [ pkgs.incus.client ];
    })

    (lib.mkIf cfg.enable {
      virtualisation.incus = {
        enable = true;
        package = pkgs.incus;
        softDaemonRestart = true;
      };
      systemd.services.incus = melinoeLib.afterMeshAddress;
      melinoe.firewall = {
        nodePorts.tcp = [ 8008 ];
        openPorts.tcp = [ 8069 ];
      };
      users.users.melinoe.extraGroups = [ "incus-admin" ];
    })
  ];
}
