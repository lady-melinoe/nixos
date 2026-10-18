{
  lib,
  pkgs,
  inputs,
  ...
}:
{
  imports = [
    ./disk-config.nix
    ./desktop.nix
    ./fingerprint.nix
    ./plymouth.nix
    inputs.lanzaboote.nixosModules.lanzaboote
  ];

  melinoe.home-manager.melinoe = [ ../../home-manager/morrigan/melinoe ];

  boot.loader.grub.enable = false;
  boot.loader.systemd-boot.enable = false;
  boot.loader.efi.efiSysMountPoint = "/boot/efi";
  boot.lanzaboote = {
    enable = true;
    pkiBundle = "/var/lib/sbctl";
  };
  environment.systemPackages = [ pkgs.sbctl ];

  nixpkgs.config.allowUnfreePredicate = pkg: builtins.elem (lib.getName pkg) [ "spotify" ];

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
  melinoe.other.incus.isVMHost = lib.mkDefault false;
  melinoe.other.update.isRemoteUpdatable = lib.mkDefault false;
  melinoe.networking.haproxy.enable = lib.mkDefault false;
  melinoe.node.id = 8;
  networking.hostName = "morrigan";
  networking.hosts = {
    "0.0.0.0" = [
      "paradise-s1.battleye.com"
      "test-s1.battleye.com"
      "paradiseenhanced-s1.battleye.com"
    ];
  };
  hardware.enableRedistributableFirmware = true;
  hardware.cpu.amd.updateMicrocode = true;
  boot.initrd.availableKernelModules = [
    "nvme"
    "xhci_pci"
    "thunderbolt"
    "usb_storage"
    "uas"
    "sd_mod"
  ];
  boot.kernelModules = [
    "kvm-amd"
    "i2c-dev"
  ];

  services.tlp = {
    enable = true;
    settings = {
      CPU_SCALING_GOVERNOR_ON_AC = "balanced";
      CPU_SCALING_GOVERNOR_ON_BAT = "balanced";
      STOP_CHARGE_THRESH_BAT0 = "1"; # Lenovo conservation mode
      DEVICES_TO_ENABLE_ON_STARTUP = "wifi bluetooth";
    };
  };

  melinoe.networking.uplinks = {
    enabled = true;
    interfaces = [
      {
        dhcp = true;
        wifi = true;
        iface = [ "wlan0" ];
      }
    ];
  };
  melinoe.services.melnode.kernelDataplane.enable = true;
  melinoe.services.melnode.dataplane = "kernel";
  melinoe.node.networking.peers = [
    {
      id = 1;
      prependCount = 4;
    }
    {
      id = 3;
      prependCount = 4;
    }
    {
      id = 5;
      prependCount = 4;
    }
    {
      id = 7;
      prependCount = 4;
    }
  ];
}
