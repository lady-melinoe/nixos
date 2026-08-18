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
    inputs.disko.nixosModules.disko
    ./disk-config.nix
  ];
  melinoe.node.id = 8;
  networking.hostName = "satet";
  melinoe.node.regions = [
    "UCC"
    "PERTH"
    "AUSTRALIA"
  ];
  melinoe.node.legacyBoot = true;
  melinoe.node.networking.uplinks = [
    {
      ip = "130.95.13.233/32";
      pub_ip = "130.95.13.233/32";
      iface = [ "enp2s0f0" ];
      subnet = "130.95.13.128/25";
      gateway = "130.95.13.129";
    }
  ];
  melinoe.node.networking.melinoe-route.extraRoutes = [ "130.95.13.0/24" ];
  melinoe.node.networking.peers = [
    {
      id = 5;
    }
    {
      id = 6;
    }
    {
      id = 7;
    }
    {
      id = 3;
    }
    {
      id = 4;
    }
    {
      id = 1;
    }
    {
      id = 2;
    }
  ];
  melinoe.node.isBuildServer = true;
}
