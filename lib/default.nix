{ lib }:
{
  importDir =
    dir:
    map (name: dir + "/${name}") (
      builtins.attrNames (
        lib.filterAttrs (
          name: type: type == "regular" && lib.hasSuffix ".nix" name && name != "default.nix"
        ) (builtins.readDir dir)
      )
    );

  nodesWith =
    dir: file:
    lib.mapAttrs (name: _: dir + "/${name}/${file}") (
      lib.filterAttrs (
        name: type: type == "directory" && builtins.pathExists (dir + "/${name}/${file}")
      ) (builtins.readDir dir)
    );
}
