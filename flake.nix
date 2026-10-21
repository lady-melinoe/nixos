{
  description = "NixOS configs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    # Pinned by rev so `nix flake update` never bumps the kernel (and thus
    # never forces a melnode module rebuild). To update: change the rev.
    nixpkgs-kernel.url = "github:NixOS/nixpkgs/4975466d324710c576dc11ad614684e6bd8cad8e";

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

    melinoe-vpn-userspace = {
      url = "git+https://gitlab.ucc.asn.au/melinoe/melinoe-vpn-userspace.git";
      flake = false;
    };

    melinoe-vpn-kernelspace = {
      url = "git+https://gitlab.ucc.asn.au/melinoe/melinoe-vpn-kernelspace.git";
      flake = false;
    };
  };

  outputs = inputs: import ./flake inputs;
}
