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
  melinoe.node.id = 2;
  networking.hostName = "hecate";
  melinoe.node.regions = [
    "UCC"
    "PERTH"
    "AUSTRALIA"
  ];
  melinoe.node.networking.uplinks = [
    {
      ip = "130.95.13.237/32";
      pub_ip = "130.95.13.237/32";
      iface = [ "enp4s0" ];
      subnet = "130.95.13.128/25";
      gateway = "130.95.13.129";
    }
    {
      ip = "198.19.0.2/32";
      iface = [
        "enp5s0f0"
        "enp5s0f1"
      ];
      bondMode = "lacp";
      lacpRate = "fast";
      subnet = "198.19.0.0/24";
      gateway = null;
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
  ];
  melinoe.node.isBuildServer = true;
}
