{ pkgs, ... }:
{
  # Session: greetd autologs melinoe straight into Hyprland (hyprlock is
  # launched from hyprland.lua), same as the old /etc/start-hypr on void.
  # UWSM provides graphical-session.target so the home-manager user services
  # (hypridle, hyprpaper, mako, hyprpolkitagent) are bound to the session.
  programs.hyprland = {
    enable = true; # also installs xdg-desktop-portal-hyprland
    withUWSM = true;
  };
  services.greetd = {
    enable = true;
    settings.default_session = {
      command = "uwsm start hyprland-uwsm.desktop";
      user = "melinoe";
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
