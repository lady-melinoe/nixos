{ ... }:
{
  imports = [
    ./ghostty.nix
    ./hypr.nix
    ./keychain.nix
    ./shell.nix
  ];

  home.username = "melinoe";
  home.homeDirectory = "/home/melinoe";
  home.stateVersion = "25.05";
}
