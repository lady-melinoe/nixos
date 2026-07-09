{
  config,
  pkgs,
  lib,
  ...
}:
let
  dir = ./.;
  nixFiles = builtins.attrNames (
    lib.filterAttrs (
      name: type: type == "regular" && lib.hasSuffix ".nix" name && name != "default.nix"
    ) (builtins.readDir dir)
  );
in
{
  imports = map (file: ./${file}) nixFiles;
}
