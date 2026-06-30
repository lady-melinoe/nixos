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
  melinoe.nodeId = 5;
  melinoe.regions = [
    "UCC"
    "PERTH"
    "AUSTRALIA"
  ];
  networking.hostName = "arke";
  melinoe.internet = [
    {
      ip = "130.95.13.219/32";
      pub_ip = "130.95.13.219/32";
      iface = [ "ens18" ];
      subnet = "130.95.13.128/25";
      gateway = "130.95.13.129";
    }
    {
      ip = "198.19.0.5/32";
      iface = [ "ens19" ];
      subnet = "198.19.0.0/24";
      gateway = null;
    }
  ];
  melinoe.advertisedRoutes = [ "130.95.13.0/24" ];

  melinoe.peers = [
    {
      id = 4;
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
}
