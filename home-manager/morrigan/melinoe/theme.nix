{ pkgs, ... }:
{
  xdg.dataFile."themes/Dracula-dark".source = "${pkgs.dracula-theme}/share/themes/Dracula";

  gtk = {
    enable = true;
    theme.name = "Dracula-dark";
  };

  dconf.settings."org/gnome/desktop/interface".color-scheme = "prefer-dark";
}
