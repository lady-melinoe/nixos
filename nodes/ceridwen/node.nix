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
  melinoe.nodeId = 3;
  melinoe.regions = [
    "UCC"
    "PERTH"
    "AUSTRALIA"
  ];
  melinoe.extraSerial = [
    1
  ];
  networking.hostName = "ceridwen";
  melinoe.internet = [
    {
      ip = "130.95.13.198/32";
      pub_ip = "130.95.13.198/32";
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

  melinoe.advertisedRoutes = [ "130.95.13.0/24" ];

  melinoe.peers = [
    {
      id = 5;
    }
    {
      id = 2;
      endpoint = "198.19.0.2";
    }
    {
      id = 4;
      endpoint = "198.19.0.4";
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
}
