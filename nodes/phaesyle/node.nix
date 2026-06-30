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

  melinoe.nodeId = 1;
  melinoe.regions = [
    "BINARYLANE"
    "PERTH"
    "AUSTRALIA"
  ];
  networking.hostName = "phaesyle";
  melinoe.internet = [
    {
      ip = "103.249.239.233/32";
      pub_ip = "103.249.239.233/32";
      iface = [ "ens3" ];
      subnet = "103.249.239.0/24";
      gateway = "103.249.239.1";
    }
  ];

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
      id = 4;
    }
  ];
}
