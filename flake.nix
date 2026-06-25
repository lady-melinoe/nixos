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

      melssh = import ./melssh.nix {
        inherit pkgs lib nixosConfigurations;
      };

      meldeploy = import ./meldeploy.nix {
        inherit pkgs lib nixosConfigurations;
      };

    in
    {
      formatter.${system} = pkgs.nixfmt-tree;

      packages.${system} =
        {
          borg-beta = pkgs.stdenvNoCC.mkDerivation {
            pname = "borg-beta";
            version = "2.0.0b21";

            src = pkgs.fetchurl {
              url = "https://github.com/borgbackup/borg/releases/download/2.0.0b21/borg-linux-glibc235-x86_64-gh";
              hash = "sha256-HN5TudJIwaYA32+TjuoyB/JxQb4OjXqAVC+o+QHyIcU=";
            };

            dontUnpack = true;
            dontBuild = true;

            installPhase = ''
              install -Dm755 "$src" "$out/bin/borg"
            '';
          };
        }
        // melssh
        // meldeploy;

      apps.${system} = {
        mel-ssh-host-ca = {
          type = "app";
          program = "${melssh.mel-ssh-host-ca}/bin/mel-ssh-host-ca";
          meta.description = "Melinoe SSH host CA management tool";
        };

        mel-deploy = {
          type = "app";
          program = "${meldeploy.mel-deploy}/bin/mel-deploy";
          meta.description = "Melinoe fleet deployment helper (parses [DEPLOY] commit tags)";
        };
      };

      inherit nixosConfigurations;
    };
}
