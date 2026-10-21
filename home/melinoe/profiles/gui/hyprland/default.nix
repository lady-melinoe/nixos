{
  config,
  lib,
  pkgs,
  ...
}:
let
  # nixpkgs still pins the pre-Lua rev (1dec300); this is the first master that
  # speaks `hyprctl eval` / hl.* for Hyprland's Lua config. Drop once nixpkgs catches up.
  iio-hyprland = pkgs.iio-hyprland.overrideAttrs (_: {
    version = "0-unstable-2026-09-07";
    src = pkgs.fetchFromGitHub {
      owner = "JeanSchoeller";
      repo = "iio-hyprland";
      rev = "8f562190a057c0b2b18dc703041da1a4a447a2a6";
      hash = "sha256-RrknSGqeQH5vGnOnhoj13/tQ1AD07oz+xGMcvAnPkvc=";
    };
  });
in
{
  imports = [ ./lock.nix ];

  assertions = [
    {
      assertion = builtins.all (pkg: builtins.elem pkg config.home.packages) [
        pkgs.librewolf
        pkgs.rofi
      ];
      message = "hyprland: librewolf and rofi are launched by the binds and must stay in home.packages (see ../packages.nix)";
    }
  ];

  home.packages = with pkgs; [
    gtk3
    psmisc
    wl-clipboard
    playerctl
    brightnessctl
    activate-linux
    iio-hyprland
    wvkbd

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

  xdg.configFile =
    lib.genAttrs
      [
        "hypr/hyprland.lua"
        "hypr/hyprland-binds.lua"
        "hypr/hyprland-de-setup.lua"
      ]
      (path: {
        source = ./. + "/${baseNameOf path}";
      });

  services = {
    hyprpolkitagent.enable = true;
    mako.enable = true;

    hyprpaper = {
      enable = true;
      settings = {
        wallpaper =
          map
            (monitor: {
              inherit monitor;
              path = "${../../../../../assets/splash.png}";
            })
            [
              "eDP-1"
              "HDMI-A-1"
            ];
        ipc = "on";
        splash = false;
      };
    };
  };
}
