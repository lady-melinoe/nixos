{
  config,
  pkgs,
  ...
}:
{
  # UWSM provides graphical-session.target so the home-manager user services
  # (hypridle, hyprpaper, mako, hyprpolkitagent) are bound to the session.
  programs.hyprland = {
    enable = true; # also installs xdg-desktop-portal-hyprland
    withUWSM = true;
  };

  systemd.services."getty@tty1".enable = false;
  systemd.services."autovt@tty1".enable = false;

  systemd.services.hypr-session = {
    wantedBy = [ "multi-user.target" ];
    after = [
      "systemd-user-sessions.service"
      "systemd-logind.service"
    ];
    conflicts = [ "tty-login.service" ];
    onSuccess = [ "tty-login.service" ];
    onFailure = [ "tty-login.service" ];
    startLimitIntervalSec = 0;
    serviceConfig = {
      User = "melinoe";
      PAMName = "login";
      TTYPath = "/dev/tty1";
      TTYReset = true;
      TTYVHangup = true;
      StandardInput = "tty";
      StandardOutput = "journal";
      StandardError = "journal";
      UtmpIdentifier = "tty1";
      UtmpMode = "user";
      ExecStart = "${pkgs.bashInteractive}/bin/bash -l -c 'exec uwsm start hyprland-uwsm.desktop -- -- --locked-cmd hyprlock'";
    };
  };

  systemd.services.tty-login = {
    after = [
      "systemd-user-sessions.service"
      "systemd-logind.service"
    ];
    conflicts = [ "hypr-session.service" ];
    onSuccess = [ "hypr-session.service" ];
    onFailure = [ "hypr-session.service" ];
    startLimitIntervalSec = 0;
    serviceConfig = {
      ExecStart = "${pkgs.util-linux.bin}/sbin/agetty --login-program ${config.services.getty.loginProgram} --noclear tty1 linux";
      TTYPath = "/dev/tty1";
      TTYReset = true;
      TTYVHangup = true;
      TTYVTDisallocate = true;
      StandardInput = "tty";
      UtmpIdentifier = "tty1";
      IgnoreSIGPIPE = false;
      SendSIGHUP = true;
    };
  };

  programs.dconf.enable = true;

  security.pam.services.hyprlock = { };
  security.pam.services.hyprlock.fprintAuth = false;
  services.pipewire = {
    enable = true;
    alsa.enable = true;
    pulse.enable = true;
  };
  services.upower.enable = true; # hyprlock battery label
  hardware.sensor.iio.enable = true; # iio-hyprland

  hardware.bluetooth.enable = true; # bar's Bluetooth module (Quickshell.Bluetooth, bluetoothctl)

  fonts.packages = [
    pkgs.noto-fonts
    pkgs.nerd-fonts.symbols-only # bar icons ("Symbols Nerd Font")
    pkgs.inter # bar tooltips
  ];
}
