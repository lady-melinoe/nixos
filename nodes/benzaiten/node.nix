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
    ../../common/shared.nix
    ../../common/bootloader.nix
    ../../common/node-opts.nix
    ../../common/incus.nix
    ../../common/networking.nix
    ../../common/settings.nix
    ../../common/users.nix
    ../../common/container-backup.nix
    ../../common/monitoring.nix
    ../../common/helpers.nix
    ./disk-config.nix
  ];
  melinoe.nodeId = 4;
  melinoe.extraSerial = [
    0
    1
    2
    3
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
      ip = "198.19.0.${toString config.melinoe.nodeId}/32";
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
      endpoint = "arke.infra.melinoe.xyz";
    }
    {
      id = 2;
      endpoint = "198.19.0.2";
    }
    {
      id = 6;
      endpoint = "lachesis.infra.melinoe.xyz";
    }
    {
      id = 7;
      endpoint = "atropos.infra.melinoe.xyz";
    }
    {
      id = 3;
      endpoint = "198.19.0.3";
    }
    {
      id = 1;
      endpoint = "phaesyle.infra.melinoe.xyz";
    }
  ];
}
