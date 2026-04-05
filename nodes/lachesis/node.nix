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
    ../../common/update.nix
    ./disk-config.nix
  ];

  melinoe.serialMode = true;
  melinoe.nodeId = 4;
  networking.hostName = "lachesis";
  melinoe.sshHostKeyPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEaUb7HXvGZXhsa9dre8X/6MGctmbCCkQ+YnELtYviKb root@hypnos";
  melinoe.sshHostCertPub = "ssh-ed25519-cert-v01@openssh.com AAAAIHNzaC1lZDI1NTE5LWNlcnQtdjAxQG9wZW5zc2guY29tAAAAIISl1efhTELyBwMeFawrt1dScxAY7RDAPNyTsz96HpuBAAAAIEaUb7HXvGZXhsa9dre8X/6MGctmbCCkQ+YnELtYviKbAAAAAAAAAAAAAAACAAAACGxhY2hlc2lzAAAAVgAAAAhsYWNoZXNpcwAAABpsYWNoZXNpcy5pbmZyYS5tZWxpbm9lLnh5egAAAAoxOTguMTkuMS40AAAACjE5OC4xOC4wLjQAAAAMMTk4LjUxLjEwMC40AAAAAGm7ijcAAAAAfIjetwAAAAAAAAAAAAAAAAAAADMAAAALc3NoLWVkMjU1MTkAAAAg9u/g9YKYQtPhfF68Iv6TxGeOvCA59QtcaBZCxUlSD1MAAABTAAAAC3NzaC1lZDI1NTE5AAAAQFj72PDcGXM6ztYLlKbcPjZ7bPov44UYeptrSW/LiTBVj165qf6RGJNEEveuusvdtq3bpeT7FqeT/ggqq3P17gE= root@hypnos";
  melinoe.internet = [
    {
      ip = "198.19.1.4/32";
      iface = [ "ens3" ];
      subnet = "198.19.1.0/24";
      gateway = "198.19.1.1";
    }
  ];

  melinoe.peers = [
    {
      id = 5;
      endpoint = "198.19.1.5";
    }
    {
      id = 2;
      endpoint = "arke.infra.melinoe.xyz";
    }
    {
      id = 7;
      endpoint = "benzaiten.infra.melinoe.xyz";
    }
    {
      id = 6;
      endpoint = "ceridwen.infra.melinoe.xyz";
    }
    {
      id = 3;
      endpoint = "hecate.infra.melinoe.xyz";
    }
    {
      id = 8;
      endpoint = "phaesyle.infra.melinoe.xyz";
    }
  ];
}
