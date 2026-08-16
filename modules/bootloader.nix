{
  config,
  lib,
  ...
}:
{
  config = {
    boot.loader.grub = lib.mkMerge [
      {
        extraConfig = lib.optionalString config.melinoe.node.serialConsoleMode ''
          serial --speed=115200 --unit=0 --word=8 --parity=no --stop=1
          terminal_input serial
          terminal_output serial
        '';
      }
      (lib.mkIf (!config.melinoe.node.legacyBoot) {
        efiSupport = true;
        efiInstallAsRemovable = true;
        device = "nodev";
      })
      (lib.mkIf config.melinoe.node.legacyBoot {
        enable = true;
      })
    ];
    boot.loader.efi.efiSysMountPoint = lib.mkIf (!config.melinoe.node.legacyBoot) "/boot/efi";
    boot.kernelParams = lib.optional config.melinoe.node.serialConsoleMode "console=ttyS0,115200n8";
  };
}
