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

  melinoe.serialMode = true;
  melinoe.nodeId = 6;
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
      endpoint = "arke.infra.melinoe.xyz";
    }
    {
      id = 4;
      endpoint = "benzaiten.infra.melinoe.xyz";
    }
    {
      id = 3;
      endpoint = "ceridwen.infra.melinoe.xyz";
    }
    {
      id = 2;
      endpoint = "hecate.infra.melinoe.xyz";
    }
    {
      id = 1;
      endpoint = "phaesyle.infra.melinoe.xyz";
    }
  ];
}
