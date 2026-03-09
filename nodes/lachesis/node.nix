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
    ../../common/monitoring.nix
    ../../common/wireguard.nix
    ../../common/uplink.nix
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

  networking.hostName = "lachesis";
  networking.domain = "";
  networking.useDHCP = false;
  networking.interfaces.ens3.useDHCP = false;

  systemd.oomd.enable = false;

  services.qemuGuest.enable = true;

  melinoe.inetIfs = [ ];
  melinoe.nodeId = 4;
  melinoe.incusDefaultStorageSource = "/array/incus/";
  melinoe.internet = [
    {
      ip = "198.19.1.4/32";
      iface = [ "ens3" ];
      subnet = "198.19.1.0/24";
      gateway = "198.19.1.1";
    }
  ];

  melinoe.wgPeers = [
    {
      id = 5;
      endpoint = "198.19.1.5";
    }
    {
      id = 2;
      endpoint = "arke.infra.melinoe.xyz";
    }
    {
      id = 7;
      endpoint = "benzaiten.infra.melinoe.xyz";
    }
    {
      id = 6;
      endpoint = "ceridwen.infra.melinoe.xyz";
    }
    {
      id = 3;
      endpoint = "hecate.infra.melinoe.xyz";
    }
    {
      id = 8;
      endpoint = "phaesyle.infra.melinoe.xyz";
    }
  ];
}
