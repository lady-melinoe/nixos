{
  config,
  pkgs,
  lib,
  ...
}:

{
  imports = [
    ./public-fetch.nix
    ./bootloader.nix
    ./node-opts.nix
    ./networking.nix
    ./melinoe-route.nix
    ./settings.nix
    ./helpers.nix
    ./distributed-build.nix
  ];
}
