{ lib, ... }:
{
  nixpkgs.config.allowUnfreePredicate =
    pkg:
    builtins.elem (lib.getName pkg) [
      "IPMIView"
      "spotify"
      "steam"
      "steam-unwrapped"
      "xone-dongle-firmware"
    ];
}
