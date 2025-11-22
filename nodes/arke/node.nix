{ config, pkgs, lib, inputs, modulesPath, ... }:
{
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    inputs.disko.nixosModules.disko
    ../../common/node-opts.nix
    ../../common/overlay.nix
    ../../common/underlay.nix
    ../../common/settings.nix
    ../../common/users.nix
    ./disk-config.nix
    ./underlay.nix
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
      address = "198.19.0.${toString melinoe.nodeId}";
      prefixLength = 24;
    }];
  };

  systemd.oomd.enable = false;

  services.qemuGuest.enable = true;

  melinoe.nodeId = 2;
}
