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
    ../../common/helpers.nix
    ./disk-config.nix
  ];

  melinoe.serialMode = true;
  melinoe.nodeId = 7;
  networking.hostName = "atropos";
  melinoe.sshHostKeyPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICfpGK2VmmUnpdezvnj+8CsJ4AHeMTKC5gVoyjU9Xqlh root@thanatos";
  melinoe.sshHostCertPub = "ssh-ed25519-cert-v01@openssh.com AAAAIHNzaC1lZDI1NTE5LWNlcnQtdjAxQG9wZW5zc2guY29tAAAAIJgr18mK5Uy5lF995QwGM6Z7tbuVs7sdKo9dlFuUbuwYAAAAICfpGK2VmmUnpdezvnj+8CsJ4AHeMTKC5gVoyjU9XqlhAAAAAAAAAAAAAAACAAAAB2F0cm9wb3MAAABUAAAAB2F0cm9wb3MAAAAZYXRyb3Bvcy5pbmZyYS5tZWxpbm9lLnh5egAAAAoxOTguMTkuMS41AAAACjE5OC4xOC4wLjUAAAAMMTk4LjUxLjEwMC41AAAAAGm7iiIAAAAAfIjeogAAAAAAAAAAAAAAAAAAADMAAAALc3NoLWVkMjU1MTkAAAAg9u/g9YKYQtPhfF68Iv6TxGeOvCA59QtcaBZCxUlSD1MAAABTAAAAC3NzaC1lZDI1NTE5AAAAQGDOfGfg67S0AUmYiTWLjri7xGXc6co9G49UkrViW/x/H45egSHakOUX5dfPBcJRbW9NNgjwxDpKP9HAWTmhoQI= root@thanatos";
  melinoe.internet = [
    {
      ip = "198.19.1.5/32";
      pub_ip = "161.33.74.225/32";
      iface = [ "ens3" ];
      subnet = "198.19.1.0/24";
      gateway = "198.19.1.1";
    }
  ];

  melinoe.peers = [
    {
      id = 6;
      endpoint = "198.19.1.4";
    }
    {
      id = 4;
      endpoint = "benzaiten.infra.melinoe.xyz";
    }
    {
      id = 2;
      endpoint = "arke.infra.melinoe.xyz";
    }
    {
      id = 9;
      endpoint = "ceridwen.infra.melinoe.xyz";
    }
    {
      id = 3;
      endpoint = "hecate.infra.melinoe.xyz";
    }
    {
      id = 1;
      endpoint = "phaesyle.infra.melinoe.xyz";
    }
  ];
}
