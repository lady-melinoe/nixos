{ pkgs, inputs, ... }:
{
  imports = [
    ./disk-config.nix
    ./desktop.nix
    inputs.lanzaboote.nixosModules.lanzaboote
  ];

  melinoe.home-manager.melinoe = [ ../../home-manager/morrigan/melinoe ];

  boot.loader.grub.enable = false;
  boot.loader.systemd-boot.enable = false;
  boot.lanzaboote = {
    enable = true;
    pkiBundle = "/var/lib/sbctl";
  };
  environment.systemPackages = [ pkgs.sbctl ];

  nix.distributedBuilds = true;
  nix.settings.builders-use-substitutes = true;
  melinoe.node.remoteBuildOn = [
    {
      hostName = "hecate.infra.melinoe.xyz";
      publicHostCA = "@cert-authority *.infra.melinoe.xyz,198.18.0.*,198.19.0.*,198.19.1.*, ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
      sshKey = "/root/.ssh/id_remotebuild";
      sshUser = "remotebuild";
      system = "x86_64-linux";
      supportedFeatures = [
        "nixos-test"
        "big-parallel"
        "kvm"
      ];
    }
    {
      hostName = "ceridwen.infra.melinoe.xyz";
      publicHostCA = "@cert-authority *.infra.melinoe.xyz,198.18.0.*,198.19.0.*,198.19.1.*, ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
      sshKey = "/root/.ssh/id_remotebuild";
      sshUser = "remotebuild";
      system = "x86_64-linux";
      supportedFeatures = [
        "nixos-test"
        "big-parallel"
        "kvm"
      ];
    }
    {
      hostName = "benzaiten.infra.melinoe.xyz";
      publicHostCA = "@cert-authority *.infra.melinoe.xyz,198.18.0.*,198.19.0.*,198.19.1.*, ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
      sshKey = "/root/.ssh/id_remotebuild";
      sshUser = "remotebuild";
      system = "x86_64-linux";
      supportedFeatures = [
        "nixos-test"
        "big-parallel"
        "kvm"
      ];
    }
  ];
  melinoe.node.type = "workstation";
  melinoe.node.id = 8;
  networking.hostName = "morrigan";

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
      STOP_CHARGE_THRESH_BAT0 = "1"; # Lenovo conservation mode
      DEVICES_TO_ENABLE_ON_STARTUP = "wifi bluetooth";
    };
  };

  networking.wireless.iwd = {
    enable = true;
    settings = {
      General.EnableNetworkConfiguration = false;
      DriverQuirks.DefaultInterface = "*";
    };
  };
  melinoe.node.networking.uplinks = [
    {
      dhcp = true;
      iface = [ "wlan0" ];
    }
  ];
  melinoe.services.melnode.kernelDataplane.enable = true;
  melinoe.services.melnode.dataplane = "kernel";
  melinoe.node.networking.peers = [
    {
      id = 1;
      prependCount = 4;
    }
    {
      id = 2;
      prependCount = 4;
    }
    {
      id = 3;
      prependCount = 4;
    }
    {
      id = 4;
      prependCount = 4;
    }
    {
      id = 5;
      prependCount = 4;
    }
    {
      id = 6;
      prependCount = 4;
    }
    {
      id = 7;
      prependCount = 4;
    }
  ];
}
