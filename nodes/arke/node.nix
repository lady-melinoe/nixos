{
  config,
  pkgs,
  lib,
  inputs,
  modulesPath,
  ...
}:
{
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    ./disk-config.nix
  ];
  nix.distributedBuilds = true;
  nix.settings.builders-use-substitutes = true;
  melinoe.other.distributedBuild.remoteBuildOn = [
    {
      id = 10; # nephthys
      sshKey = "/root/.ssh/id_remotebuild";
    }
    {
      id = 3; # ceridwen
      sshKey = "/root/.ssh/id_remotebuild";
    }
    {
      id = 4; # benzaiten
      sshKey = "/root/.ssh/id_remotebuild";
    }
  ];
  melinoe.node.id = 5;
  boot.initrd.availableKernelModules = [
    "ata_piix"
    "uhci_hcd"
    "xen_blkfront"
    "vmw_pvscsi"
    "sd_mod"
    "usbhid"
    "usb_storage"
    "mpt3sas"
    "ehci_pci"
  ];
  boot.initrd.kernelModules = [
    "nvme"
    "kvm-intel"
  ];
  boot.loader.grub = {
    efiSupport = true;
    efiInstallAsRemovable = true;
    device = "nodev";
  };
  boot.loader.efi.efiSysMountPoint = "/boot/efi";
  networking.hostName = "arke";
  melinoe.networking.uplinks = {
    enabled = true;
    interfaces = [
      {
        ip = "130.95.13.236/32";
        pub_ip = "130.95.13.236/32";
        iface = [ "ens18" ];
        subnet = "130.95.13.128/25";
        gateway = "130.95.13.129";
      }
    ];
  };
  melinoe.services.melnode.extraRoutes = [ "130.95.13.0/24" ];
  melinoe.services.melnode.kernelDataplane.enable = true;
  melinoe.services.melnode.dataplane = "kernel";
  melinoe.node.networking.peers = [
    {
      id = 1;
      prependCount = 1;
    }
    {
      id = 3;
    }
    {
      id = 4;
    }
    {
      id = 6;
      prependCount = 2;
    }
    {
      id = 7;
      prependCount = 2;
    }
    {
      id = 8;
      prependCount = 4;
    }
    {
      id = 9;
    }
    {
      id = 10;
    }
  ];
}
