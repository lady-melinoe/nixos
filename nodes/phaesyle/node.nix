{ config, pkgs, lib, inputs, modulesPath, ... }:

{
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    inputs.disko.nixosModules.disko
    ../../common/node-opts.nix
    ../../common/overlay.nix
    ../../common/underlay.nix
    ../../common/settings.nix
    ../../common/firewall.nix
    ../../common/users.nix
    ../../common/container-backup.nix
    ../../common/wireguard.nix
    ./disk-config.nix
  ];

  boot.loader.grub = {
    enable = true;
    device = /dev/sda;
    configurationLimit = 20;
  };

  boot.initrd.availableKernelModules = [ "ata_piix" "uhci_hcd" "xen_blkfront" "vmw_pvscsi" ];
  boot.initrd.kernelModules = [ "nvme" ];

  networking.hostName = "phaesyle";
  networking.domain = "";
  networking.useDHCP = false;
  networking.interfaces.ens3.useDHCP = true;

  systemd.oomd.enable = false;

  services.qemuGuest.enable = true;

  melinoe.inetIfs = [ "ens3" ];
  melinoe.nodeId = 8;
  melinoe.incusDefaultStorageSource = "/array/incus/";

  melinoe.wgPeers = [
    {
      id = 7;
      endpoint = "benzaiten.infra.melinoe.xyz";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
    {
      id = 2;
      endpoint = "arke.infra.melinoe.xyz";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
    {
      id = 6;
      endpoint = "ceridwen.infra.melinoe.xyz";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
    {
      id = 3;
      endpoint = "hecate.infra.melinoe.xyz";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
  ];
}
