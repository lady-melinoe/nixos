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
  melinoe.node.id = 7;
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
  networking.hostName = "atropos";
  boot.loader.grub = {
    efiSupport = true;
    efiInstallAsRemovable = true;
    device = "nodev";
    extraConfig = ''
      serial --speed=115200 --unit=0 --word=8 --parity=no --stop=1
      terminal_input serial
      terminal_output serial
    '';
  };
  boot.loader.efi.efiSysMountPoint = "/boot/efi";
  boot.kernelParams = [ "console=ttyS0,115200n8" ];
  melinoe.services.melnode.kernelDataplane.enable = true;
  melinoe.services.melnode.dataplane = "kernel";
  melinoe.networking.uplinks = {
    enabled = true;
    interfaces = [
      {
        ip = "198.19.1.5/32";
        pub_ip = "161.33.74.225/32";
        iface = [ "ens3" ];
        subnet = "198.19.1.0/24";
        gateway = "198.19.1.1";
      }
    ];
  };
  melinoe.node.networking.peers = [
    {
      id = 1;
      prependCount = 2;
    }
    {
      id = 3;
      prependCount = 2;
    }
    {
      id = 4;
      prependCount = 2;
    }
    {
      id = 5;
      prependCount = 2;
    }
    {
      id = 6;
    }
    {
      id = 8;
      prependCount = 4;
    }
    {
      id = 9;
      prependCount = 2;
    }
    {
      id = 10;
      prependCount = 2;
    }
  ];
}
