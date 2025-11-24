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

  networking.hostName = "arke";
  networking.domain = "";
  networking.useDHCP = false;
  networking.interfaces.ens18.useDHCP = true;
  networking.interfaces.ens19 = {
    useDHCP = false;
    ipv4.addresses = [{
      address = "198.19.0.${toString config.melinoe.nodeId}";
      prefixLength = 24;
    }];
  };

  systemd.oomd.enable = false;

  services.qemuGuest.enable = true;

  melinoe.inetIfs = "ens18";
  melinoe.p2pIfs = "ens19";
  melinoe.nodeId = 2;
  melinoe.bgpPeers = [
    { id = 3; addr = "198.19.0.3"; }
    { id = 6; addr = "198.19.0.6"; }
    { id = 7; addr = "198.19.0.7"; }
  ];
  melinoe.incusDefaultStorageSource = "/array/incus/";
}
