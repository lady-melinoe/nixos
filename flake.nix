{
  description = "NixOS configs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    # Pinned by rev so `nix flake update` never bumps the kernel (and thus
    # never forces a kernel or melinoe-vpn module rebuild). To update: change the rev.
    nixpkgs-kernel.url = "github:NixOS/nixpkgs/1a08097fc1ed85575db39763b2c4a5d8970a37ba";

    disko = {
      url = "github:nix-community/disko";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    home-manager = {
      url = "github:nix-community/home-manager";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    lanzaboote = {
      url = "github:nix-community/lanzaboote";
      inputs.nixpkgs.follows = "nixpkgs";
    };

    melinoe-vpn-kmod = {
      url = "git+https://gitlab.ucc.asn.au/melinoe/melinoe-vpn-kmod.git";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = inputs: import ./flake inputs;
}
