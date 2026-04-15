{ config, pkgs, lib, inputs, modulesPath, ... }:

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
  melinoe.nodeId = 3;
  networking.hostName = "ceridwen";
  melinoe.sshHostKeyPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBRZL0L9G/uir/cLeUB1UoZI9UK0gSk1KxuWO8+Dm8Da root@ceridwen";
  melinoe.sshHostCertPub = "ssh-ed25519-cert-v01@openssh.com AAAAIHNzaC1lZDI1NTE5LWNlcnQtdjAxQG9wZW5zc2guY29tAAAAIPo3GLnqnIgCES77FW/8NF7fZufJpCPPZEzgnU/Xo0kUAAAAIBRZL0L9G/uir/cLeUB1UoZI9UK0gSk1KxuWO8+Dm8DaAAAAAAAAAAAAAAACAAAACGNlcmlkd2VuAAAAZwAAAAhjZXJpZHdlbgAAABpjZXJpZHdlbi5pbmZyYS5tZWxpbm9lLnh5egAAAA0xMzAuOTUuMTMuMTk4AAAACjE5OC4xOS4wLjYAAAAKMTk4LjE4LjAuNgAAAAwxOTguNTEuMTAwLjYAAAAAabuKLAAAAAB8iN6sAAAAAAAAAAAAAAAAAAAAMwAAAAtzc2gtZWQyNTUxOQAAACD27+D1gphC0+F8Xrwi/pPEZ468IDn1C1xoFkLFSVIPUwAAAFMAAAALc3NoLWVkMjU1MTkAAABAzd67R5jbgO6X+LexfHVbk0qjyVBy3O16Mv1sX4dLZ0JY+Y1FoE9DVoked7GArVs9yFnRgMzz9JlRos/OwHQnAw== root@ceridwen";
  melinoe.internet = [
    {
      ip = "130.95.13.198/32";
      pub_ip = "130.95.13.198/32";
      iface = [ "eno1" ];
      subnet = "130.95.13.128/25";
      gateway = "130.95.13.129";
    }
    {
      ip = "198.19.0.${toString config.melinoe.nodeId}/32";
      iface = [ "eno3" "eno4" ];
      bondMode = "lacp";
      lacpRate = "fast";
      subnet = "198.19.0.0/24";
      gateway = null;
    }
  ];

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
      id = 4;
      endpoint = "198.19.0.4";
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
      id = 1;
      endpoint = "phaesyle.infra.melinoe.xyz";
    }
  ];
}
