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
    {
      id = 1;
      prependCount = 1;
    }
    {
      id = 3;
    }
    {
      id = 4;
    }
    {
      id = 5;
    }
    {
      id = 6;
      prependCount = 2;
    }
    {
      id = 7;
      prependCount = 2;
    }
    {
      id = 10;
    }
  ];
  melinoe.node.isBuildServer = true;
}
