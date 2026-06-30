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

  melinoe.nodeId = 2;
  networking.hostName = "hecate";
  melinoe.regions = [
    "UCC"
    "PERTH"
    "AUSTRALIA"
  ];
  melinoe.internet = [
    {
      ip = "130.95.13.224/32";
      pub_ip = "130.95.13.224/32";
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

  melinoe.advertisedRoutes = [ "130.95.13.0/24" ];

  melinoe.peers = [
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
      endpoint = "198.19.0.3";
    }
    {
      id = 4;
      endpoint = "198.19.0.4";
    }
    {
      id = 1;
    }
  ];
}
