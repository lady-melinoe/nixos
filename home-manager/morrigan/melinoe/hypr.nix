{
  config,
  lib,
  pkgs,
  ...
}:
let
  splash = ../../../common/assets/splash.png;
  playerctlExe = lib.getExe pkgs.playerctl;
  hyprlockExe = lib.getExe pkgs.hyprlock;
  loginctl = "${pkgs.systemd}/bin/loginctl";
in
{
  assertions = [
    {
      assertion = builtins.all (pkg: builtins.elem pkg config.home.packages) [
        pkgs.librewolf
        pkgs.rofi
      ];
      message = "hypr.nix: librewolf and rofi are launched by the Hyprland binds and must stay in home.packages (see packages.nix)";
    }
  ];

  home.packages = with pkgs; [
    # launched/toggled from hyprland binds & autostart
    gtk3 # gtk-launch
    psmisc # killall
    wl-clipboard
    playerctl
    brightnessctl
    activate-linux
    iio-hyprland
    wvkbd

    # theming referenced by hyprland-de-setup.lua
    qt6Packages.qt6ct
    dracula-theme
  ];

  home.pointerCursor = {
    enable = true;
    name = "Adwaita";
    package = pkgs.adwaita-icon-theme;
    size = 24;
    hyprcursor.enable = true;
  };

  xdg.configFile = {
    "hypr/hyprland.lua".source = ./hypr/hyprland.lua;
    "hypr/hyprland-binds.lua".source = ./hypr/hyprland-binds.lua;
    "hypr/hyprland-de-setup.lua".source = ./hypr/hyprland-de-setup.lua;
  };

  services.hyprpolkitagent.enable = true;

  services.mako.enable = true;

  services.hypridle = {
    enable = true;
    settings = {
      general = {
        lock_cmd = "${playerctlExe} -a pause & ${hyprlockExe} --grace 0 --immediate-render --no-fade-in";
        before_sleep_cmd = "${playerctlExe} -a pause & ${hyprlockExe} --grace 0 --immediate-render --no-fade-in && sleep 1";
        ignore_dbus_inhibit = false;
        ignore_systemd_inhibit = false;
      };
      listener = [
        {
          timeout = 600;
          on-timeout = "${loginctl} lock-session";
        }
      ];
    };
  };

  services.hyprpaper = {
    enable = true;
    settings = {
      wallpaper = [
        {
          monitor = "eDP-1";
          path = "${splash}";
        }
        {
          monitor = "HDMI-A-1";
          path = "${splash}";
        }
      ];
      ipc = "on";
      splash = false;
    };
  };

  # PAM service is set up in nodes/morrigan/desktop.nix
  programs.hyprlock = {
    enable = true;
    settings = {
      general = {
        ignore_empty_input = true;
        immediate_render = true;
        fail_timeout = 1000;
      };
      background = [
        {
          monitor = "";
          path = "${splash}";
          color = "rgba(0, 0, 0, 1.0)";
          blur_passes = 2;
        }
      ];
      shape = [
        {
          monitor = "";
          size = "360, 60";
          color = "rgba(0, 0, 0, 0.0)"; # no fill
          rounding = -1; # circle
          border_size = 4;
          border_color = "rgba(0, 207, 230, 1.0)";
          position = "0, 80";
          halign = "center";
          valign = "center";
        }
      ];
      input-field = [
        {
          monitor = "";
          size = "20%, 5%";
          outline_thickness = 3;
          inner_color = "rgba(0, 0, 0, 0.0)"; # no fill
          outer_color = "rgba(33ccffee) rgba(00ff99ee) 45deg";
          check_color = "rgba(00ff99ee) rgba(ff6633ee) 120deg";
          fail_color = "rgba(ff6633ee) rgba(ff0066ee) 40deg";
          font_color = "rgb(143, 143, 143)";
          fade_on_empty = false;
          rounding = 15;
          position = "0, -20";
          halign = "center";
          valign = "center";
        }
      ];
      label = [
        {
          monitor = "";
          text = "Hi there, $USER";
          color = "rgba(200, 200, 200, 1.0)";
          font_size = 25;
          font_family = "Noto Sans";
          position = "0, 80";
          halign = "center";
          valign = "center";
        }
        {
          monitor = "";
          text = ''cmd[update:10000] echo -n "$(upower -i /org/freedesktop/UPower/devices/battery_BAT0 | grep percentage | grep -Po "\d+\%")           $TIME12"'';
          color = "rgba(10, 10, 10, 1.0)";
          font_size = 16;
          font_family = "Noto Sans";
          position = "-40, 14";
          halign = "right";
          valign = "bottom";
        }
        {
          monitor = "";
          text = "Use fingerprint";
          color = "rgba(200, 200, 200, 1.0)";
          font_size = 16;
          font_family = "Noto Sans";
          position = "0, -100";
          halign = "center";
          valign = "center";
          onclick = "timeout 5 fprintd-verify melinoe && pkill -USR1 hyprlock";
        }
      ];
      auth.pam = {
        enabled = true;
        module = "hyprlock";
      };
      auth.fingerprint.enabled = false;
    };
  };
}
