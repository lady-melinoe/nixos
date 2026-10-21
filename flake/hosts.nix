{ inputs, system }:
let
  inherit (inputs.nixpkgs) lib;
  melinoeLib = import ../lib { inherit lib; };

  hostNames = lib.attrNames (
    lib.filterAttrs (_: type: type == "directory") (builtins.readDir ../hosts)
  );
in
lib.genAttrs hostNames (
  name:
  lib.nixosSystem {
    specialArgs = {
      inherit inputs melinoeLib;
      profiles = melinoeLib.namedFilesIn ../profiles;
    };

    modules = [
      inputs.disko.nixosModules.disko
      {
        nixpkgs.hostPlatform = system;
        networking.hostName = name;
      }
      ../cluster
      { melinoe.cluster.nodes = lib.genAttrs hostNames (host: import ../hosts/${host}/public.nix); }
      ../modules
      ../hosts/${name}
    ];
  }
)
