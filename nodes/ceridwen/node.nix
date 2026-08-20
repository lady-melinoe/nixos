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
  melinoe.node.id = 3;
  melinoe.node.regions = [
    "UCC"
    "PERTH"
    "AUSTRALIA"
  ];
  melinoe.extraSerial = [
    1
  ];
  networking.hostName = "ceridwen";
  melinoe.node.networking.uplinks = [
    {
      ip = "130.95.13.235/32";
      pub_ip = "130.95.13.235/32";
      iface = [ "eno1" ];
      subnet = "130.95.13.128/25";
      gateway = "130.95.13.129";
    }
    {
      ip = "198.19.0.3/32";
      iface = [
        "eno3"
        "eno4"
      ];
      bondMode = "lacp";
      lacpRate = "fast";
      subnet = "198.19.0.0/24";
      gateway = null;
    }
  ];
  melinoe.services.melinoe-route.extraRoutes = [ "130.95.13.0/24" ];
  melinoe.node.networking.peers = [
    {
      id = 5;
    }
    {
      id = 2;
    }
    {
      id = 4;
    }
    {
      id = 6;
    }
    {
      id = 7;
    }
    {
      id = 1;
    }
  ];
  melinoe.node.isBuildServer = true;
}
