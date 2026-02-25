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
    in {
      nixosConfigurations = {
        arke = lib.nixosSystem {
          inherit system;
          specialArgs = { inherit inputs system; };
          modules = [ ./nodes/arke/node.nix ];
        };

        hecate = lib.nixosSystem {
          inherit system;
          specialArgs = { inherit inputs system; };
          modules = [ ./nodes/hecate/node.nix ];
        };

        ceridwen = lib.nixosSystem {
          inherit system;
          specialArgs = { inherit inputs system; };
          modules = [ ./nodes/ceridwen/node.nix ];
        };

        benzaiten = lib.nixosSystem {
          inherit system;
          specialArgs = { inherit inputs system; };
          modules = [ ./nodes/benzaiten/node.nix ];
        };

        lachesis = lib.nixosSystem {
          inherit system;
          specialArgs = { inherit inputs system; };
          modules = [ ./nodes/lachesis/node.nix ];
        };

        atropos = lib.nixosSystem {
          inherit system;
          specialArgs = { inherit inputs system; };
          modules = [ ./nodes/atropos/node.nix ];
        };

        phaesyle = lib.nixosSystem {
          inherit system;
          specialArgs = { inherit inputs system; };
          modules = [ ./nodes/phaesyle/node.nix ];
        };
      };

      deploy.nodes = {
        arke = {
          hostname = "arke";
          profiles.system = {
            user = "root";
            path = deploy-rs.lib.x86_64-linux.activate.nixos self.nixosConfigurations.arke;
          };
        };

        hecate = {
          hostname = "hecate";
          profiles.system = {
            user = "root";
            path = deploy-rs.lib.x86_64-linux.activate.nixos self.nixosConfigurations.hecate;
          };
        };

        ceridwen = {
          hostname = "ceridwen";
          profiles.system = {
            user = "root";
            path = deploy-rs.lib.x86_64-linux.activate.nixos self.nixosConfigurations.ceridwen;
          };
        };

        benzaiten = {
          hostname = "benzaiten";
          profiles.system = {
            user = "root";
            path = deploy-rs.lib.x86_64-linux.activate.nixos self.nixosConfigurations.benzaiten;
          };
        };

        lachesis = {
          hostname = "lachesis";
          profiles.system = {
            user = "root";
            path = deploy-rs.lib.x86_64-linux.activate.nixos self.nixosConfigurations.lachesis;
          };
        };

        atropos = {
          hostname = "atropos";
          profiles.system = {
            user = "root";
            path = deploy-rs.lib.x86_64-linux.activate.nixos self.nixosConfigurations.atropos;
          };
        };
        phaesyle = {
          hostname = "phaesyle";
          profiles.system = {
            user = "root";
            path = deploy-rs.lib.x86_64-linux.activate.nixos self.nixosConfigurations.phaesyle;
          };
        };
      };
    };
}
