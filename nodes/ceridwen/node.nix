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
    ./disk-config.nix
  ];
  melinoe.node.id = 3;
  boot.loader.grub = {
    efiSupport = true;
    efiInstallAsRemovable = true;
    device = "nodev";
  };
  boot.loader.efi.efiSysMountPoint = "/boot/efi";
  systemd.services."serial-getty@ttyS1" = {
    enable = true;
    wantedBy = [ "getty.target" ];
    overrideStrategy = "asDropin";
    serviceConfig.ExecStart = lib.mkForce [
      ""
      "${pkgs.util-linux}/bin/agetty --login-program ${pkgs.shadow}/bin/login --noclear -L ttyS1 115200 vt220"
    ];
  };
  networking.hostName = "ceridwen";
  melinoe.networking.uplinks = {
    enabled = true;
    interfaces = [
      {
        ip = "130.95.13.235/32";
        pub_ip = "130.95.13.235/32";
        iface = [ "eno1" ];
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
      id = 4;
    }
    {
      id = 5;
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
  melinoe.node.isBuildServer = true;
}
