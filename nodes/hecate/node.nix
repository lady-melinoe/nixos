{ config, pkgs, lib, inputs, modulesPath, ... }:

{
  imports = [
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

  networking.hostName = "hecate";
  networking.domain = "";
  networking.useDHCP = false;
  networking.interfaces.enp4s0.useDHCP = false;
  networking.interfaces.enp5s0f0.useDHCP = false;
  networking.interfaces.enp5s0f1.useDHCP = false;

  environment.systemPackages = with pkgs; [
    python3
    vorbis-tools
  ];
  networking.bonds.bond0 = {
    interfaces = [ "enp5s0f0" "enp5s0f1" ];
    driverOptions = {
      mode = "802.3ad";
      lacp_rate = "fast";
    };
  };
  networking.interfaces.bond0 = {
    useDHCP = false;
    ipv4.addresses = [{
      address = "198.19.0.${toString config.melinoe.nodeId}";
      prefixLength = 24;
    }];
  };

  boot.loader.grub = {
    efiSupport = true;
    configurationLimit = 20;
    efiInstallAsRemovable = true;
    device = "nodev";
  };

  boot.loader.efi.efiSysMountPoint = "/boot/efi";
  boot.initrd.availableKernelModules = [ "ata_piix" "uhci_hcd" "xen_blkfront" "vmw_pvscsi" ];
  boot.initrd.kernelModules = [ "nvme" ];

  melinoe.inetIfs = [ "bond0" "enp5s0f0" "enp5s0f1" ];
  melinoe.nodeId = 3;
  melinoe.incusDefaultStorageSource = "/array/incus/";
  melinoe.internet = [
    {
      ip = "130.95.13.134/32";
      iface = [ "enp4s0" ];
      subnet = "130.95.13.128/25";
      gateway = "130.95.13.129";
    }
  ];

  melinoe.wgPeers = [
    {
      id = 2;
      endpoint = "198.19.0.2";
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
