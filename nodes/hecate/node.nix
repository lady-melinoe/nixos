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
    ./disk-config.nix
  ];

  melinoe.nodeId = 3;
  networking.hostName = "hecate";
  melinoe.sshHostKeyPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIp9/tXauAG0KCl68JUOE8Yp8rYgpnK4uUtLmxvEVdwx root@hecate";
  melinoe.sshHostCertPub = "ssh-ed25519-cert-v01@openssh.com AAAAIHNzaC1lZDI1NTE5LWNlcnQtdjAxQG9wZW5zc2guY29tAAAAIKg407AsYbWr5/Hchro26jcEUGscT3Mua3jC3COJkySSAAAAIIp9/tXauAG0KCl68JUOE8Yp8rYgpnK4uUtLmxvEVdwxAAAAAAAAAAAAAAACAAAABmhlY2F0ZQAAAGMAAAAGaGVjYXRlAAAAGGhlY2F0ZS5pbmZyYS5tZWxpbm9lLnh5egAAAA0xMzAuOTUuMTMuMTM0AAAACjE5OC4xOS4wLjMAAAAKMTk4LjE4LjAuMwAAAAwxOTguNTEuMTAwLjMAAAAAabuKMQAAAAB8iN6xAAAAAAAAAAAAAAAAAAAAMwAAAAtzc2gtZWQyNTUxOQAAACD27+D1gphC0+F8Xrwi/pPEZ468IDn1C1xoFkLFSVIPUwAAAFMAAAALc3NoLWVkMjU1MTkAAABAZ/ZBI9N9qnhf6CaKQ2oak+4bbSR5WI+yPHD54gaD1HoqAe75dK8cV4fpR9Q+F64Q4kystCUwKfVYU36JcStLAA== root@hecate";
  melinoe.internet = [
    {
      ip = "130.95.13.134/32";
      iface = [ "enp4s0" ];
      subnet = "130.95.13.128/25";
      gateway = "130.95.13.129";
    }
    {
      ip = "198.19.0.${toString config.melinoe.nodeId}/32";
      iface = [ "enp5s0f0" "enp5s0f1" ];
      bondMode = "lacp";
      lacpRate = "fast";
      subnet = "198.19.0.0/24";
      gateway = null;
    }
  ];

  melinoe.peers = [
    {
      id = 2;
      endpoint = "arke.infra.melinoe.xyz";
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
      endpoint = "198.19.0.6";
    }
    {
      id = 7;
      endpoint = "198.19.0.7";
    }
    {
      id = 8;
      endpoint = "phaesyle.infra.melinoe.xyz";
    }
  ];
}
