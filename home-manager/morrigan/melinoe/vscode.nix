{ pkgs, ... }:
{
  programs.vscodium = {
    enable = true;
    mutableExtensionsDir = true; # default, shown for clarity
  };
}
