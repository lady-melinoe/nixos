{
  description = "NixOS configs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    disko.url = "github:nix-community/disko";
    deploy-rs.url = "github:serokell/deploy-rs";
  };

  outputs = { self, nixpkgs, disko, deploy-rs, ... }@inputs:
    let
      system = "x86_64-linux";
      lib = nixpkgs.lib;
      pkgs = import nixpkgs {
        inherit system;
      };

      nodeDir = ./nodes;
      nodes =
        let
          dirEntries = builtins.readDir nodeDir;
          nodeNames = lib.filterAttrs (name: type: type == "directory" && builtins.pathExists (nodeDir + "/${name}/node.nix")) dirEntries;
        in lib.mapAttrs (name: _: nodeDir + "/${name}/node.nix") nodeNames;

      mkNixosConfiguration = _: module: lib.nixosSystem {
        inherit system;
        specialArgs = { inherit inputs system; };
        modules = [ module ];
      };

      mkDeployNode = name: _: {
        hostname = "${name}.infra.melinoe.xyz";
        profiles.system = {
          user = "root";
          path = deploy-rs.lib.${system}.activate.nixos self.nixosConfigurations.${name};
        };
      };
    in {
      packages.${system} = {
        tunudp = pkgs.stdenv.mkDerivation {
          pname = "tunudp";
          version = "0.1.0";

          src = ./.;
          dontUnpack = true;

          nativeBuildInputs = [ pkgs.pkg-config ];
          buildInputs = [ pkgs.liburing ];

          buildPhase = ''
            runHook preBuild
            cc -O3 -pthread $(pkg-config --cflags liburing) "$src/pkg_dump/tunudp/tunudp.c" \
              $(pkg-config --libs liburing) -o tunudp
            runHook postBuild
          '';

          installPhase = ''
            runHook preInstall
            install -Dm755 tunudp "$out/bin/tunudp"
            runHook postInstall
          '';
        };

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
            runHook preInstall
            install -Dm755 "$src" "$out/bin/borg"
            runHook postInstall
          '';
        };
      };

      nixosConfigurations = lib.mapAttrs mkNixosConfiguration nodes;

      deploy.nodes = lib.mapAttrs mkDeployNode nodes;
    };
}
