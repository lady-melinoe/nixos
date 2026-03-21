{ config, lib, ... }:
{
  boot.loader.grub = lib.mkMerge [
    {
      configurationLimit = 20;
      extraConfig = lib.optionalString config.melinoe.serialMode ''
        serial --speed=115200 --unit=0 --word=8 --parity=no --stop=1
        terminal_input serial
        terminal_output serial
      '';
    }
    (lib.mkIf (!config.melinoe.legacyBoot) {
      efiSupport = true;
      efiInstallAsRemovable = true;
      device = "nodev";
    })
    (lib.mkIf config.melinoe.legacyBoot {
      enable = true;
    })
  ];

  boot.loader.efi.efiSysMountPoint = lib.mkIf (!config.melinoe.legacyBoot) "/boot/efi";
  boot.kernelParams = lib.optional config.melinoe.serialMode "console=ttyS0,115200n8";
}
