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
    ../../common
    ./disk-config.nix
  ];
  melinoe.nodeId = 4;
  melinoe.regions = [
    "UCC"
    "PERTH"
    "AUSTRALIA"
  ];
  melinoe.extraSerial = [
    1
  ];
  networking.hostName = "benzaiten";
  melinoe.internet = [
    {
      ip = "130.95.13.133/32";
      pub_ip = "130.95.13.133/32";
      iface = [ "eno1" ];
      subnet = "130.95.13.128/25";
      gateway = "130.95.13.129";
    }
    {
      ip = "198.19.0.4/32";
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
  melinoe.advertisedRoutes = [ "130.95.13.0/24" ];
  melinoe.peers = [
    {
      id = 5;
    }
    {
      id = 2;
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
      id = 1;
    }
  ];
  melinoe.isBuildServer = true;
}
