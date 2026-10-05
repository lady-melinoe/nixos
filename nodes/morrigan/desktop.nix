{
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

  # Same as void: greetd runs this as melinoe (no greeter user, no login) ->
  # Hyprland (locked by hyprlock on start) -> when it exits, agreety asks for a
  # password and gives a shell -> when that shell ends, greetd restarts this and
  # we're back in Hyprland.
  services.greetd = {
    enable = true;
    settings = rec {
      initial_session = {
      command = pkgs.writeShellScript "start-hypr" ''
        while true; do
        uwsm start hyprland-uwsm.desktop
        sleep 1
        ${pkgs.greetd}/bin/agreety --cmd ${pkgs.bashInteractive}/bin/bash
        sleep 1
        done
      '';
        user = "melinoe";
      };
      default_session = initial_session;
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
