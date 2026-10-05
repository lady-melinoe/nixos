{ ... }:
{
  # Also provides `kitten`, used for the dropdown terminal panel
  # (started in hyprland-de-setup.lua, toggled from the bar and SUPER+t).
  programs.kitty = {
    enable = true;
    settings = {
      background_opacity = 0.9;
      background_blur = 0;
      dynamic_background_opacity = true;
      term = "kitty";
      kitty_mod = "ctrl+shift";
    };
    keybindings = {
      "kitty_mod+a>m" = "set_background_opacity +0.2";
      "kitty_mod+a>l" = "set_background_opacity -0.2";
    };
  };
}
