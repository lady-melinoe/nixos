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
  melinoe.nodeId = 2;
  networking.hostName = "arke";
  melinoe.sshHostKeyPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMDqg2R4xx9aE6MntTKbprB2hD9zArE2AR9WBz3ZTEpC root@arke";
  melinoe.sshHostCertPub = "ssh-ed25519-cert-v01@openssh.com AAAAIHNzaC1lZDI1NTE5LWNlcnQtdjAxQG9wZW5zc2guY29tAAAAIHqf7ovtf7RC+gAanQj7K+Pe0eaYIl0DhRVnW7sKsSZBAAAAIMDqg2R4xx9aE6MntTKbprB2hD9zArE2AR9WBz3ZTEpCAAAAAAAAAAAAAAACAAAABGFya2UAAABfAAAABGFya2UAAAAWYXJrZS5pbmZyYS5tZWxpbm9lLnh5egAAAA0xMzAuOTUuMTMuMjE5AAAACjE5OC4xOS4wLjIAAAAKMTk4LjE4LjAuMgAAAAwxOTguNTEuMTAwLjIAAAAAabuKGwAAAAB8iN6bAAAAAAAAAAAAAAAAAAAAMwAAAAtzc2gtZWQyNTUxOQAAACD27+D1gphC0+F8Xrwi/pPEZ468IDn1C1xoFkLFSVIPUwAAAFMAAAALc3NoLWVkMjU1MTkAAABAi/Cx2PDt2ivgQVOLSfO0A02bgU5tnyIy3LzP7igHDlJ3IbiFqvnGs2Ssn+8qpanYf0mPhQ1u9G+PaJaqME1+Bg== root@arke";
  melinoe.internet = [
    {
      ip = "130.95.13.219/32";
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

  melinoe.peers = [
    {
      id = 7;
      endpoint = "benzaiten.infra.melinoe.xyz";
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
      id = 8;
      endpoint = "phaesyle.infra.melinoe.xyz";
    }
  ];
}
