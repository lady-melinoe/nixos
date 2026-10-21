{ ... }:
{
  programs.ghostty = {
    enable = true;
    # shell-integration is disabled below, so don't inject it via bashrc either
    enableBashIntegration = false;

    settings = {
      background = "#000000";
      background-opacity = 0.9;
      background-blur = false;
      theme = "Kitty Default";
      font-family = "Noto Sans Mono";
      font-size = 16;
      term = "ghostty";
      shell-integration = "none";
      #     gtk-adwaita = false; seems to be broken on nix? idk.
      gtk-titlebar = false;
      gtk-tabs-location = "hidden";
      window-theme = "system";
      gtk-wide-tabs = false;
      confirm-close-surface = false;
    };
  };

  # Kitty's default colours (Arch's copy; Ghostty doesn't ship it)
  xdg.configFile."ghostty/themes/Kitty Default".source = ./ghostty/Kitty-Default;
}
