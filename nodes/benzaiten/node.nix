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
  melinoe.nodeId = 7;
  networking.hostName = "benzaiten";
  melinoe.sshHostKeyPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEvhNzfpuzpfATP4LS7Ptt245vlb0lPIp8wSDW9VyVUT root@benzaiten";
  melinoe.sshHostCertPub = "ssh-ed25519-cert-v01@openssh.com AAAAIHNzaC1lZDI1NTE5LWNlcnQtdjAxQG9wZW5zc2guY29tAAAAICD9bhi0PwfO+j6ithkqSmvCX3gbxjCpLA1wX3GH7j2QAAAAIEvhNzfpuzpfATP4LS7Ptt245vlb0lPIp8wSDW9VyVUTAAAAAAAAAAAAAAACAAAACWJlbnphaXRlbgAAAGkAAAAJYmVuemFpdGVuAAAAG2JlbnphaXRlbi5pbmZyYS5tZWxpbm9lLnh5egAAAA0xMzAuOTUuMTMuMTMzAAAACjE5OC4xOS4wLjcAAAAKMTk4LjE4LjAuNwAAAAwxOTguNTEuMTAwLjcAAAAAAAAAAP//////////AAAAAAAAAAAAAAAAAAAAMwAAAAtzc2gtZWQyNTUxOQAAACD27+D1gphC0+F8Xrwi/pPEZ468IDn1C1xoFkLFSVIPUwAAAFMAAAALc3NoLWVkMjU1MTkAAABANj4ea+J4QgxRys3McAYDcC2iksvyXaioWLcH+KSCJUCE8IQx0pMVBaXqhpEjGah8XkgHu9AiQIn92s8DyEWOCQ== root@benzaiten";
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
      iface = [ "eno3" "eno4" ];
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
      id = 3;
      endpoint = "198.19.0.3";
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
      id = 8;
      endpoint = "phaesyle.infra.melinoe.xyz";
    }
  ];
}
