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

  networking.hostName = "benzaiten";
  networking.domain = "";
  networking.useDHCP = false;
  networking.interfaces.eno1.useDHCP = false;
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

  melinoe.inetIfs = [ "bond0" "eno3" "eno4" ];
  melinoe.nodeId = 7;
  melinoe.incusDefaultStorageSource = "/array/incus/";
  melinoe.internet = [
    {
      ip = "130.95.13.133/32";
      iface = "eno1";
      subnet = "130.95.13.128/25";
      gateway = "130.95.13.129";
    }
  ];

  melinoe.wgPeers = [
    {
      id = 5;
      endpoint = "atropos.infra.melinoe.xyz";
    }
    {
      id = 2;
      endpoint = "198.19.0.2";
    }
    {
      id = 4;
      endpoint = "lachesis.infra.melinoe.xyz";
    }
    {
      id = 6;
      endpoint = "198.19.0.6";
    }
    {
      id = 8;
      endpoint = "phaesyle.infra.melinoe.xyz";
    }
  ];
}
