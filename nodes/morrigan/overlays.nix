{ lib, ... }:
{
  nixpkgs.config.allowUnfreePredicate =
    pkg:
    builtins.elem (lib.getName pkg) [
      "spotify"
      "steam"
      "steam-unwrapped"
    ];

  # TODO: drop once NixOS/nixpkgs#576062 reaches nixos-unstable
  nixpkgs.overlays = [
    (final: prev: {
      calibre = prev.calibre.override {
        speechd-minimal = prev.speechd-minimal.overrideAttrs (old: {
          nativeBuildInputs = old.nativeBuildInputs ++ [ final.python3 ];
        });
      };
    })
  ];
}
