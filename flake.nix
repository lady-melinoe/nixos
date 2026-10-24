{
  description = "NixOS configs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    # Pinned by rev so `nix flake update` never bumps the kernel (and thus
    # never forces a kernel or bpfx module rebuild). To update: change the rev.
    nixpkgs-kernel.url = "github:NixOS/nixpkgs/02c087f3fe673c9e317f206a627cb6b910336dbe";

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

    bpfx-kernel = {
      url = "git+https://gitlab.ucc.asn.au/melinoe/bpfx-kernel.git";
      flake = false;
    };
  };

  outputs = inputs: import ./flake inputs;
}
