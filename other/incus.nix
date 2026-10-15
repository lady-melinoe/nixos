{
  config,
  lib,
  pkgs,
  melinoeLib,
  ...
}:
let
  inherit (lib) mkIf mkOption types;
  cfg = config.melinoe.other.incus;
in
{
  options.melinoe.other.incus = {
    isVMHost = mkOption {
      type = types.bool;
      default = true;
      description = ''
        Whether this node runs Incus and hosts local containers/VMs.

        Disable for cluster nodes that should still be full mesh members
        (still receive routes to other nodes' VMs, still advertise their own
        pub_ips/extraRoutes via melnode) but never run any containers
        themselves - melnode.nix doesn't bother scanning for local vm-
        interfaces in that case, since they will never exist.
      '';
    };
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
