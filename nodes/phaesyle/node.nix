{ config, pkgs, lib, inputs, modulesPath, ... }:

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
    ./disk-config.nix
  ];

  melinoe.nodeId = 8;
  networking.hostName = "phaesyle";
  melinoe.sshHostKeyPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILJ6nAuIeJB9nySz1l9Q+8DrZzhlcEpfiOJF6gvXQh4i root@phaesyle";
  melinoe.sshHostCertPub = "ssh-ed25519-cert-v01@openssh.com AAAAIHNzaC1lZDI1NTE5LWNlcnQtdjAxQG9wZW5zc2guY29tAAAAIJu0pFJifeqQPs+XALBvGWcxp2gLIvL71xaXH8/LxYo3AAAAILJ6nAuIeJB9nySz1l9Q+8DrZzhlcEpfiOJF6gvXQh4iAAAAAAAAAAAAAAACAAAACHBoYWVzeWxlAAAAWwAAAAhwaGFlc3lsZQAAABpwaGFlc3lsZS5pbmZyYS5tZWxpbm9lLnh5egAAAA8xMDMuMjQ5LjIzOS4yMzMAAAAKMTk4LjE4LjAuOAAAAAwxOTguNTEuMTAwLjgAAAAAabuKOwAAAAB8iN67AAAAAAAAAAAAAAAAAAAAMwAAAAtzc3gtZWQyNTUxOQAAACD27+D1gphC0+F8Xrwi/pPEZ468IDn1C1xoFkLFSVIPUwAAAFMAAAALc3NoLWVkMjU1MTkAAABAv7ANIhqPsUBNdvay8Jjjb98FyBC5wzbYm2frjaDCCXiKvEurD3RbJb6frlJTkbw0GH69akHiWG9Z/+n8URjSDw== root@phaesyle";
  melinoe.internet = [
    {
      ip = "103.249.239.233/32";
      iface = [ "ens3" ];
      subnet = "103.249.239.0/24";
      gateway = "103.249.239.1";
    }
  ];

  melinoe.peers = [
    {
      id = 2;
      endpoint = "arke.infra.melinoe.xyz";
    }
    {
      id = 3;
      endpoint = "hecate.infra.melinoe.xyz";
    }
    {
      id = 4;
      endpoint = "lachesis.infra.melinoe.xyz";
    }
    {
      id = 5;
      endpoint = "atropos.infra.melinoe.xyz";
    }
    {
      id = 6;
      endpoint = "ceridwen.infra.melinoe.xyz";
    }
    {
      id = 7;
      endpoint = "benzaiten.infra.melinoe.xyz";
    }
  ];
}
