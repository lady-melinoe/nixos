{ ... }:
{
  programs.rofi = {
    enable = true;
    settings = {
      show-icons = true;
      display-drun = "";
      disable-history = false;
    };
    theme = ./rofi/dracula.rasi;
  };
}
