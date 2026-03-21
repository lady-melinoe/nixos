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
  melinoe.nodeId = 7;
  networking.hostName = "benzaiten";
  melinoe.sshHostKeyPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEvhNzfpuzpfATP4LS7Ptt245vlb0lPIp8wSDW9VyVUT root@benzaiten";
  melinoe.sshHostCertPub = "ssh-ed25519-cert-v01@openssh.com AAAAIHNzaC1lZDI1NTE5LWNlcnQtdjAxQG9wZW5zc2guY29tAAAAIGKIDBJzydlOzN61aJ51Q0vOkVQTFG/0Qak8/34PXZZMAAAAIEvhNzfpuzpfATP4LS7Ptt245vlb0lPIp8wSDW9VyVUTAAAAAAAAAAAAAAACAAAACWJlbnphaXRlbgAAAGkAAAAJYmVuemFpdGVuAAAAG2JlbnphaXRlbi5pbmZyYS5tZWxpbm9lLnh5egAAAA0xMzAuOTUuMTMuMTMzAAAACjE5OC4xOS4wLjcAAAAKMTk4LjE4LjAuNwAAAAwxOTguNTEuMTAwLjcAAAAAabuKJgAAAAB8iN6mAAAAAAAAAAAAAAAAAAAAMwAAAAtzc3gtZWQyNTUxOQAAACD27+D1gphC0+F8Xrwi/pPEZ468IDn1C1xoFkLFSVIPUwAAAFMAAAALc3NoLWVkMjU1MTkAAABAVTvsG4swFsXsrYEswozMBh6aEhbO8bImDIhzIfsPX/u2MMhwkovnrd4Tkt68tjkqjUCVX0dsXzDa5gE5OV6aDQ== root@benzaiten";
  melinoe.internet = [
    {
      ip = "130.95.13.133/32";
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
      endpoint = "198.19.0.6";
    }
    {
      id = 8;
      endpoint = "phaesyle.infra.melinoe.xyz";
    }
  ];
}
