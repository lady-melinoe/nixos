{ lib, pkgs, ... }:
{
  xdg.dataFile."themes/Dracula-dark".source = "${pkgs.dracula-theme}/share/themes/Dracula";

  gtk = {
    enable = true;
    theme.name = "Dracula-dark";
    iconTheme = {
      name = "Dracula";
      package = pkgs.dracula-icon-theme;
    };
  };

  xdg.configFile."qt6ct/qt6ct.conf".text = lib.generators.toINI { } {
    Appearance.icon_theme = "Dracula";
  };

  dconf.settings."org/gnome/desktop/interface".color-scheme = "prefer-dark";
}
