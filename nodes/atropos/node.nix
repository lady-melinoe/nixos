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
    (modulesPath + "/profiles/qemu-guest.nix")
    inputs.disko.nixosModules.disko
    ../../common
    ./disk-config.nix
  ];

  melinoe.serialMode = true;
  melinoe.nodeId = 7;
  melinoe.regions = [
    "ORACLE"
    "MELBOURNE"
    "AUSTRALIA"
  ];
  networking.hostName = "atropos";
  melinoe.internet = [
    {
      ip = "198.19.1.5/32";
      pub_ip = "161.33.74.225/32";
      iface = [ "ens3" ];
      subnet = "198.19.1.0/24";
      gateway = "198.19.1.1";
    }
  ];

  melinoe.peers = [
    {
      id = 6;
      endpoint = "198.19.1.4";
    }
    {
      id = 4;
    }
    {
      id = 5;
    }
    {
      id = 3;
    }
    {
      id = 2;
    }
    {
      id = 1;
    }
  ];
}
