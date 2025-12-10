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
    ./disk-config.nix
  ];

  networking.hostName = "hecate";
  networking.domain = "";
  networking.useDHCP = false;
  networking.interfaces.enp4s0.useDHCP = true;
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

  melinoe.inetIfs = "enp4s0";
  melinoe.p2pIfs = "bond0";
  melinoe.nodeId = 3;
  melinoe.bgpPeers = [
    { id = 2; addr = "198.19.0.2"; }
    { id = 6; addr = "198.19.0.6"; }
    { id = 7; addr = "198.19.0.7"; }
  ];
  melinoe.incusDefaultStorageSource = "/array/incus/";
}
