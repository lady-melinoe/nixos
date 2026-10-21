{ lib, ... }:
{
  nixpkgs.config.allowUnfreePredicate =
    pkg:
    builtins.elem (lib.getName pkg) [
      "spotify"
      "steam"
      "steam-unwrapped"
    ];

  # TODO: drop once NixOS/nixpkgs#576062 and NixOS/nixpkgs#576063 reach nixos-unstable
  nixpkgs.overlays = [
    (final: prev: {
      calibre = prev.calibre.override {
        speechd-minimal = prev.speechd-minimal.overrideAttrs (old: {
          nativeBuildInputs = old.nativeBuildInputs ++ [ final.python3 ];
        });
      };
      pythonPackagesExtensions = prev.pythonPackagesExtensions ++ [
        (_: pyprev: {
          pyopenjtalk-plus = pyprev.pyopenjtalk-plus.overridePythonAttrs (old: {
            patches = (old.patches or [ ]) ++ [
              (final.fetchpatch {
                name = "fix-cython-3.3.0.patch";
                url = "https://github.com/tsukumijima/pyopenjtalk-plus/commit/90b644f9e9d397882e6eafe4dcd1843a4fde6a63.patch";
                includes = [ "tests/test_frontend.py" ];
                hash = "sha256-Jb7T/bRUHxqwBLwV6xYC/teaFHm5GmqJxA7sCX+oiNI=";
              })
            ];
          });
        })
      ];
    })
  ];
}
