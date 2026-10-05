{ ... }:
{
  imports = [
    ./ghostty.nix
    ./hypr.nix
    ./keychain.nix
    ./kitty.nix
    ./quickshell.nix
    ./shell.nix
  ];

  home.username = "melinoe";
  home.homeDirectory = "/home/melinoe";
  home.stateVersion = "25.05";
}
