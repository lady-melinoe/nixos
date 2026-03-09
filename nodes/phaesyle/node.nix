{ config, pkgs, lib, inputs, modulesPath, ... }:

{
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    inputs.disko.nixosModules.disko
    ../../common/node-opts.nix
    ../../common/incus.nix
    ../../common/networking.nix
    ../../common/settings.nix
    ../../common/firewall.nix
    ../../common/users.nix
    ../../common/container-backup.nix
    ../../common/monitoring.nix
    ./disk-config.nix
  ];

  boot.loader.grub = {
    enable = true;
    configurationLimit = 20;
  };

  boot.initrd.availableKernelModules = [ "ata_piix" "uhci_hcd" "xen_blkfront" "vmw_pvscsi" ];
  boot.initrd.kernelModules = [ "nvme" ];

  networking.hostName = "phaesyle";
  networking.domain = "";
  networking.useDHCP = false;

  systemd.oomd.enable = false;

  services.qemuGuest.enable = true;

  melinoe.nodeId = 8;
  melinoe.incusDefaultStorageSource = "/array/incus/";
  melinoe.internet = [
    {
      ip = "103.249.239.233/32";
      iface = [ "ens3" ];
      subnet = "103.249.239.0/24";
      gateway = "103.249.239.1";
    }
  ];

  melinoe.wgPeers = [
    {
      id = 2;
      endpoint = "arke.infra.melinoe.xyz";
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
      id = 7;
      endpoint = "benzaiten.infra.melinoe.xyz";
    }
  ];
}
