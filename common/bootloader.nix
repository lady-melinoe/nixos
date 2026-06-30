{
  config,
  pkgs,
  lib,
  ...
}:

let
  serialGetty = port: {
    name = "serial-getty@ttyS${toString port}";
    value = {
      enable = true;
      wantedBy = [ "getty.target" ];
    }
    // lib.optionalAttrs (port >= 1 && port <= 3) {
      overrideStrategy = "asDropin";
      serviceConfig.ExecStart = lib.mkForce [
        ""
        "${pkgs.util-linux}/bin/agetty --login-program ${pkgs.shadow}/bin/login --noclear -L ttyS${toString port} 115200 vt220"
      ];
    };
  };
in
{
  assertions = [
    {
      assertion = !(config.melinoe.serialMode && config.melinoe.extraSerial != [ ]);
      message = "melinoe.serialMode and melinoe.extraSerial are mutually exclusive.";
    }
  ];

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

  systemd.services = builtins.listToAttrs (map serialGetty config.melinoe.extraSerial);
}
