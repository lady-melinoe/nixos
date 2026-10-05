{ ... }:
{
  programs.rofi = {
    enable = true;
    extraConfig = {
      show-icons = true;
      display-drun = "";
      disable-history = false;
    };
    theme = ./rofi/dracula.rasi;
  };
}
