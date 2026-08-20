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
            name: type:
            type == "directory"
            && builtins.pathExists (nodeDir + "/${name}/node.nix")
          ) dirEntries;
        in
        lib.mapAttrs (
          name: _:
          nodeDir + "/${name}/node.nix"
        ) nodeNames;

      mkNixosConfiguration =
        _: module:
        lib.nixosSystem {
          inherit system;

          specialArgs = {
            inherit inputs system;
          };

          modules = [
            ./common
            ./modules
            module
          ];
        };

      nixosConfigurations =
        lib.mapAttrs mkNixosConfiguration nodes;

      mel-ssh-provision = import ./flake-helpers/mel-ssh-provision.nix {
        inherit pkgs lib nixosConfigurations;
      };

      mel-deploy = import ./flake-helpers/mel-deploy.nix {
        inherit pkgs lib nixosConfigurations;
      };
    in
    {
      formatter.${system} = pkgs.nixfmt-tree;

      apps.${system} = {
        mel-ssh-provision = {
          type = "app";
          program = "${mel-ssh-provision}/bin/mel-ssh-provision";
          meta.description = "Melinoe SSH host provisioning tool";
        };

        mel-deploy = {
          type = "app";
          program = "${mel-deploy}/bin/mel-deploy";
          meta.description = "Melinoe fleet deployment helper (parses [DEPLOY] commit tags)";
        };
      };

      inherit nixosConfigurations;
    };
}
