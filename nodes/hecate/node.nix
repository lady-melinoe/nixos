{ config, pkgs, lib, inputs, modulesPath, ... }:

{
  imports = [
    inputs.disko.nixosModules.disko
    ../../common/shared.nix
    ../../common/node-opts.nix
    ../../common/incus.nix
    ../../common/networking.nix
    ../../common/settings.nix
    ../../common/users.nix
    ../../common/container-backup.nix
    ../../common/monitoring.nix
    ./disk-config.nix
  ];

  networking.domain = "";
  environment.systemPackages = with pkgs; [
    python3
    vorbis-tools
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

  melinoe.nodeId = 3;
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

  melinoe.wgPeers = [
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
