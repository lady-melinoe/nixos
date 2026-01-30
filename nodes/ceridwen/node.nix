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
    ../../common/wireguard.nix
    ./disk-config.nix
  ];

  networking.hostName = "ceridwen";
  networking.domain = "";
  networking.useDHCP = false;
  networking.interfaces.eno1.useDHCP = true;
  networking.interfaces.eno3.useDHCP = false;
  networking.interfaces.eno4.useDHCP = false;
  networking.bonds.bond0 = {
    interfaces = [ "eno3" "eno4" ];
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
  boot.initrd.availableKernelModules = [ "sd_mod" "usbhid" "usb_storage" "mpt3sas" "ehci_pci" ];
  boot.initrd.kernelModules = [ "kvm-intel" ];

  melinoe.inetIfs = "eno1";
  melinoe.p2pIfs = "bond0";
  melinoe.nodeId = 6;
  melinoe.incusDefaultStorageSource = "/array/incus/";
  melinoe.incusRootSize = "40GiB";
  melinoe.enableIncusPreseed = false;
  melinoe.bgpPeers = [
    { id = 2; addr = "198.19.0.2"; }
    { id = 3; addr = "198.19.0.3"; }
    { id = 7; addr = "198.19.0.7"; }
  ];

  melinoe.wgPeers = [
    {
      id = 5;
      endpoint = "thanatos.infra.melinoe.xyz";
      allowedIPs = [ "198.19.1.0/24" "198.51.100.0/24" ];
    }
  ];
}
