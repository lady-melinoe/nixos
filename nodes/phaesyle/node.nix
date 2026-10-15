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
  melinoe.node.id = 1;
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
  networking.hostName = "phaesyle";
  melinoe.services.melnode.kernelDataplane.enable = true;
  melinoe.services.melnode.dataplane = "kernel";
  melinoe.networking.uplinks = {
    enabled = true;
    interfaces = [
      {
        ip = "103.249.239.233/32";
        pub_ip = "103.249.239.233/32";
        iface = [ "ens3" ];
        subnet = "103.249.239.0/24";
        gateway = "103.249.239.1";
      }
    ];
  };
  melinoe.node.networking.peers = [
    {
      id = 3;
      prependCount = 1;
    }
    {
      id = 4;
      prependCount = 1;
    }
    {
      id = 5;
      prependCount = 1;
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
      prependCount = 1;
    }
    {
      id = 10;
      prependCount = 1;
    }
  ];
}
