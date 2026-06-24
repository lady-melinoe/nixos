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
    ../../common/networking.nix
    ../../common/settings.nix
    ../../common/helpers.nix
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
      endpoint = "arke.infra.melinoe.xyz";
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
      id = 4;
      endpoint = "benzaiten.infra.melinoe.xyz";
    }
  ];
}
