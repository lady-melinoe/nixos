{
  description = "NixOS configs";
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    disko.url = "github:nix-community/disko";
  };
  outputs =
    {
      self,
      nixpkgs,
      disko,
      ...
    }@inputs:
    let
      system = "x86_64-linux";
      lib = nixpkgs.lib;
      pkgs = import nixpkgs { inherit system; };
      nodeDir = ./nodes;
      nodes =
        let
          dirEntries = builtins.readDir nodeDir;
          nodeNames = lib.filterAttrs (
            name: type: type == "directory" && builtins.pathExists (nodeDir + "/${name}/node.nix")
          ) dirEntries;
        in
        lib.mapAttrs (name: _: nodeDir + "/${name}/node.nix") nodeNames;
      mkNixosConfiguration =
        _: module:
        lib.nixosSystem {
          inherit system;
          specialArgs = { inherit inputs system; };
          modules = [ module ];
        };
      nixosConfigurations = lib.mapAttrs mkNixosConfiguration nodes;
      meltools = import ./meltools.nix {
        inherit pkgs lib nixosConfigurations;
      };
    in
    {
      formatter.${system} = pkgs.nixfmt-tree;
      apps.${system} = {
        mel-ssh-host-ca = {
          type = "app";
          program = "${meltools.mel-ssh-host-ca}/bin/mel-ssh-host-ca";
          meta.description = "Melinoe SSH host CA management tool";
        };
        mel-deploy = {
          type = "app";
          program = "${meltools.mel-deploy}/bin/mel-deploy";
          meta.description = "Melinoe fleet deployment helper (parses [DEPLOY] commit tags)";
        };
      };
      inherit nixosConfigurations;
    };
}
