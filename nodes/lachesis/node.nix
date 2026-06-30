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
  melinoe.nodeId = 6;
  melinoe.regions = [
    "ORACLE"
    "MELBOURNE"
    "AUSTRALIA"
  ];
  networking.hostName = "lachesis";
  melinoe.internet = [
    {
      ip = "198.19.1.4/32";
      pub_ip = "161.33.94.79/32";
      iface = [ "ens3" ];
      subnet = "198.19.1.0/24";
      gateway = "198.19.1.1";
    }
  ];

  melinoe.peers = [
    {
      id = 7;
      endpoint = "198.19.1.5";
    }
    {
      id = 5;
    }
    {
      id = 4;
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
