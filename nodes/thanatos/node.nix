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
    ./disk-config.nix
  ];

  boot.loader.grub = {
    efiSupport = true;
    configurationLimit = 20;
    efiInstallAsRemovable = true;
    device = "nodev";
  };
  boot.loader.efi.efiSysMountPoint = "/boot/efi";
  boot.initrd.availableKernelModules = [ "ata_piix" "uhci_hcd" "xen_blkfront" "vmw_pvscsi" ];
  boot.initrd.kernelModules = [ "nvme" ];

  networking.hostName = "thanatos";
  networking.domain = "";
  networking.useDHCP = false;
  networking.interfaces.ens3.useDHCP = true;

  systemd.oomd.enable = false;

  services.qemuGuest.enable = true;

  melinoe.inetIfs = "ens3";
  melinoe.p2pIfs = "ens3";
  melinoe.underlayPrefix = "198.19.1";
  melinoe.nodeId = 5;
  melinoe.bgpPeers = [
    { id = 4; addr = "198.19.1.4"; }
  ];
  melinoe.incusDefaultStorageSource = "/array/incus/";
}
