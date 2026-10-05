{ ... }:
{
  imports = [
    ./ghostty.nix
    ./hypr.nix
    ./keychain.nix
    ./kitty.nix
    ./packages.nix
    ./quickshell.nix
    ./rofi.nix
    ./shell.nix
    ./theme.nix
  ];

  home.username = "melinoe";
  home.homeDirectory = "/home/melinoe";
  home.stateVersion = "25.05";
}
