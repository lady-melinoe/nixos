{ lib, ... }:
let
  nodesDir = ../nodes;
  entries = builtins.readDir nodesDir;
  publicModules = lib.concatMap (
    name:
    if entries.${name} == "directory" then
      let
        candidate = nodesDir + "/${name}/public.nix";
      in
      if builtins.pathExists candidate then [ candidate ] else [ ]
    else
      [ ]
  ) (builtins.attrNames entries);
in
{
  imports = publicModules;
}
