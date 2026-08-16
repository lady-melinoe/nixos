{
  config,
  pkgs,
  lib,
  ...
}:
let
  inherit (lib) mkOption types;
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
  options.melinoe = {
    extraSerial = mkOption {
      type = types.listOf types.int;
      default = [ ];
      description = "Additional ttyS serial gettys to enable for IPMI-accessible serial consoles.";
    };
  };

  config = {
    assertions = [
      {
        assertion = !(config.melinoe.node.serialConsoleMode && config.melinoe.extraSerial != [ ]);
        message = "melinoe.node.serialConsoleMode and melinoe.extraSerial are mutually exclusive.";
      }
    ];
    systemd.services = builtins.listToAttrs (map serialGetty config.melinoe.extraSerial);
  };
}
