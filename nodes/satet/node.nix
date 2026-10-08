{
  config,
  pkgs,
  lib,
  inputs,
  modulesPath,
  ...
}:
{
  imports = [
    ./disk-config.nix
  ];
  melinoe.node.id = 9;
  melinoe.node.legacyBoot = true;
  networking.hostName = "satet";
  melinoe.node.networking.uplinks = [
    {
      ip = "130.95.13.233/32";
      pub_ip = "130.95.13.233/32";
      iface = [ "enp2s0f0" ];
      subnet = "130.95.13.128/25";
      gateway = "130.95.13.129";
    }
  ];
  melinoe.services.melnode.extraRoutes = [ "130.95.13.0/24" ];
  melinoe.services.melnode.kernelDataplane.enable = true;
  melinoe.services.melnode.dataplane = "kernel";
  melinoe.node.networking.peers = [
  ];
  melinoe.node.isBuildServer = true;
}
