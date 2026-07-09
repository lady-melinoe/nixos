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

  melinoe.serialMode = true;
  melinoe.nodeId = 6;
  melinoe.regions = [
    "ORACLE"
    "MELBOURNE"
    "AUSTRALIA"
  ];
  networking.hostName = "lachesis";
  melinoe.internet = [
    {
      ip = "198.19.1.4/32";
      pub_ip = "161.33.94.79/32";
      iface = [ "ens3" ];
      subnet = "198.19.1.0/24";
      gateway = "198.19.1.1";
    }
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

  melinoe.peers = [
    {
      id = 7;
      endpoint = "198.19.1.5";
    }
    {
      id = 5;
    }
    {
      id = 4;
    }
    {
      id = 3;
    }
    {
      id = 2;
    }
    {
      id = 1;
    }
  ];
}
