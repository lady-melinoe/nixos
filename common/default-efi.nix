{ config, lib, ... }:
{
  boot.loader.grub = {
    efiSupport = true;
    configurationLimit = 20;
    efiInstallAsRemovable = true;
    device = "nodev";
  };

  boot.loader.efi.efiSysMountPoint = "/boot/efi";
  boot.loader.grub.extraConfig = lib.optionalString config.melinoe.serialMode ''
    serial --speed=115200 --unit=0 --word=8 --parity=no --stop=1
    terminal_input serial
    terminal_output serial
  '';
  boot.kernelParams = lib.optional config.melinoe.serialMode "console=ttyS0,115200n8";
}
