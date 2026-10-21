{ nixpkgs, ... }@inputs:
let
  system = "x86_64-linux";
  inherit (nixpkgs) lib;
  pkgs = nixpkgs.legacyPackages.${system};

  nixosConfigurations = import ./hosts.nix { inherit inputs system; };
  tools = import ../tools { inherit pkgs lib nixosConfigurations; };
in
{
  inherit nixosConfigurations;

  formatter.${system} = pkgs.nixfmt-tree;

  apps.${system} = lib.mapAttrs (_: tool: {
    type = "app";
    program = lib.getExe tool;
    meta = { inherit (tool.meta) description; };
  }) tools;
}
