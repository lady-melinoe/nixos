{
  config,
  pkgs,
  lib,
  inputs,
  modulesPath,
  ...
}:

{
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    inputs.disko.nixosModules.disko
    ../../common
    ./disk-config.nix
  ];

  nix.distributedBuilds = true;
  nix.settings.builders-use-substitutes = true;
  melinoe.buildMachines = [
    {
      hostName = "hecate.infra.melinoe.xyz";
      publicHostCA = "@cert-authority *.infra.melinoe.xyz,198.18.0.*,198.19.0.*,198.19.1.*,198.19.3.*,198.51.100.*, ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
      sshKey = "/root/.ssh/id_remotebuild";
      sshUser = "remotebuild";
      system = "x86_64-linux";
      supportedFeatures = [
        "nixos-test"
        "big-parallel"
        "kvm"
      ];
    }
    {
      hostName = "ceridwen.infra.melinoe.xyz";
      publicHostCA = "@cert-authority *.infra.melinoe.xyz,198.18.0.*,198.19.0.*,198.19.1.*,198.19.3.*,198.51.100.*, ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
      sshKey = "/root/.ssh/id_remotebuild";
      sshUser = "remotebuild";
      system = "x86_64-linux";
      supportedFeatures = [
        "nixos-test"
        "big-parallel"
        "kvm"
      ];
    }
    {
      hostName = "benzaiten.infra.melinoe.xyz";
      publicHostCA = "@cert-authority *.infra.melinoe.xyz,198.18.0.*,198.19.0.*,198.19.1.*,198.19.3.*,198.51.100.*, ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
      sshKey = "/root/.ssh/id_remotebuild";
      sshUser = "remotebuild";
      system = "x86_64-linux";
      supportedFeatures = [
        "nixos-test"
        "big-parallel"
        "kvm"
      ];
    }
  ];

  melinoe.nodeId = 5;
  melinoe.regions = [
    "UCC"
    "PERTH"
    "AUSTRALIA"
  ];
  networking.hostName = "arke";
  melinoe.internet = [
    {
      ip = "130.95.13.219/32";
      pub_ip = "130.95.13.219/32";
      iface = [ "ens18" ];
      subnet = "130.95.13.128/25";
      gateway = "130.95.13.129";
    }
    {
      ip = "198.19.0.5/32";
      iface = [ "ens19" ];
      subnet = "198.19.0.0/24";
      gateway = null;
    }
  ];
  melinoe.advertisedRoutes = [ "130.95.13.0/24" ];

  melinoe.peers = [
    {
      id = 4;
    }
    {
      id = 2;
    }
    {
      id = 6;
    }
    {
      id = 7;
    }
    {
      id = 3;
    }
    {
      id = 1;
    }
  ];
}
