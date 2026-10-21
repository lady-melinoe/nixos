{ lib, pkgs, ... }:
{
  systemd.services."serial-getty@ttyS1" = {
    enable = true;
    wantedBy = [ "getty.target" ];
    overrideStrategy = "asDropin";
    serviceConfig.ExecStart = lib.mkForce [
      ""
      "${pkgs.util-linux}/bin/agetty --login-program ${pkgs.shadow}/bin/login --noclear -L ttyS1 115200 vt220"
    ];
  };
}
