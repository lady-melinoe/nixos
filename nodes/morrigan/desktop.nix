{
  config,
  lib,
  pkgs,
  utils,
  ...
}:
{
  # UWSM provides graphical-session.target so the home-manager user services
  # (hypridle, hyprpaper, mako, hyprpolkitagent) are bound to the session.
  programs.hyprland = {
    enable = true; # also installs xdg-desktop-portal-hyprland
    withUWSM = true;
  };

  # Boot -> Hyprland (unauthenticated, but hyprlock is launched on start, same
  # as void) -> when it exits/crashes, a stock agetty login prompt -> when that
  # login session ends, back to Hyprland.
  # There is deliberately no autologin getty anywhere: the only way into a shell
  # is a password at the stock agetty prompt.
  #
  # tty1-session is the only thing started at boot; it runs hyprland-tty1 and
  # getty@tty1 one after the other, forever. Both of those are manual-start
  # only (nothing wants them), and getty@tty1 doesn't restart itself, so
  # control always returns to the loop.
  systemd.defaultUnit = "graphical.target";

  systemd.services.tty1-session = {
    after = [
      "systemd-user-sessions.service"
      "systemd-logind.service"
    ];
    wantedBy = [ "graphical.target" ];
    restartIfChanged = false;
    stopIfChanged = false;
    serviceConfig = {
      Restart = "always";
      ExecStart = pkgs.writeShellScript "tty1-session" ''
        while true; do
          systemctl start --wait hyprland-tty1.service
          systemctl start --wait getty@tty1.service
          sleep 1 # don't spin if both fail immediately
        done
      '';
    };
    path = [ config.systemd.package ];
  };

  systemd.services.hyprland-tty1 = {
    after = [ "systemd-logind.service" ];
    wants = [
      "dbus.socket"
      "systemd-logind.service"
    ];
    # never share tty1 with the login prompt
    conflicts = [ "getty@tty1.service" ];

    restartIfChanged = false;
    stopIfChanged = false;
    unitConfig.ConditionPathExists = "/dev/tty1";
    environment.XDG_SESSION_TYPE = "wayland";
    serviceConfig = {
      # Login shell so uwsm gets the usual /etc/profile and ~/.profile env
      ExecStart = "${pkgs.bashInteractive}/bin/bash -lc 'exec uwsm start hyprland-uwsm.desktop'";
      User = "melinoe";
      IgnoreSIGPIPE = "no";
      # Log this user with utmp, as we replace (a)getty
      UtmpIdentifier = "%n";
      UtmpMode = "user";
      TTYPath = "/dev/tty1";
      TTYReset = "yes";
      TTYVHangup = "yes";
      TTYVTDisallocate = "yes";
      StandardInput = "tty-fail";
      StandardOutput = "journal";
      StandardError = "journal";
      # A real logind "user" class session (so `loginctl lock-session` works)
      PAMName = "hyprland-tty1";
    };
  };

  # Stock agetty, but: not started at boot, and it exits (instead of
  # respawning) when the login session ends so tty1-session can continue.
  systemd.targets.getty.wants = lib.mkForce [ ];
  systemd.services."getty@tty1" = {
    overrideStrategy = "asDropin";
    serviceConfig.Restart = "no";
  };

  # Session-only PAM stack (systemd's PAMName never authenticates anyone)
  security.pam.services.hyprland-tty1 = {
    useDefaultRules = false;
    rules = {
      auth = utils.pam.autoOrderRules [
        {
          name = "unix";
          control = "required";
          modulePath = config.security.pam.pam_unixModulePath;
          settings.nullok = true;
        }
      ];
      account = utils.pam.autoOrderRules [
        {
          name = "unix";
          control = "required";
          modulePath = config.security.pam.pam_unixModulePath;
        }
      ];
      session = utils.pam.autoOrderRules [
        {
          name = "unix";
          control = "required";
          modulePath = config.security.pam.pam_unixModulePath;
        }
        {
          name = "env";
          control = "required";
          modulePath = "${config.security.pam.package}/lib/security/pam_env.so";
          settings.conffile = "/etc/pam/environment";
          settings.readenv = 0;
        }
        {
          name = "systemd";
          control = "required";
          modulePath = "${config.systemd.package}/lib/security/pam_systemd.so";
        }
      ];
    };
  };

  security.pam.services.hyprlock = { };

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
