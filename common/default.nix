{
  config,
  pkgs,
  lib,
  ...
}:

{
  imports = [
    ./shared.nix
    ./bootloader.nix
    ./node-opts.nix
    ./networking.nix
    ./settings.nix
    ./helpers.nix
    ./distributed-build.nix
  ];
}
