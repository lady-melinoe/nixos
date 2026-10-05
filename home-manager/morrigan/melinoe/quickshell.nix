{ pkgs, ... }:
{
  programs.quickshell = {
    enable = true;
    configs.testshell = ./testshell;
    activeConfig = "testshell";
    # Started by graphical-session.target (via UWSM) instead of hyprland.lua
    systemd.enable = true;
  };

  # External tools the bar's modules shell out to (iw/ip for Network,
  # bluetoothctl comes from hardware.bluetooth, brightnessctl/killall/wvkbd
  # are in hypr.nix)
  home.packages = with pkgs; [
    iw
    impala # Network click
    bluetuith # Bluetooth click
    pulsemixer # Pulseaudio click
  ];
}
