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
    ../../common/helpers.nix
    ./disk-config.nix
  ];
  melinoe.nodeId = 5;
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
      ip = "198.19.0.${toString config.melinoe.nodeId}/32";
      iface = [ "ens19" ];
      subnet = "198.19.0.0/24";
      gateway = null;
    }
  ];
  melinoe.advertisedRoutes = [ "130.95.13.0/24" ];

  melinoe.peers = [
    {
      id = 4;
      endpoint = "benzaiten.infra.melinoe.xyz";
    }
    {
      id = 2;
      endpoint = "hecate.infra.melinoe.xyz";
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
      endpoint = "ceridwen.infra.melinoe.xyz";
    }
    {
      id = 1;
      endpoint = "phaesyle.infra.melinoe.xyz";
    }
  ];
}
