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
    ./disk-config.nix
  ];
  nix.distributedBuilds = true;
  nix.settings.builders-use-substitutes = true;
  melinoe.node.remoteBuildOn = [
    {
      hostName = "hecate.infra.melinoe.xyz";
      publicHostCA = "@cert-authority *.infra.melinoe.xyz,198.18.0.*,198.19.0.*,198.19.1.*, ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
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
      publicHostCA = "@cert-authority *.infra.melinoe.xyz,198.18.0.*,198.19.0.*,198.19.1.*, ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
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
      publicHostCA = "@cert-authority *.infra.melinoe.xyz,198.18.0.*,198.19.0.*,198.19.1.*, ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
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
  melinoe.node.id = 5;
  networking.hostName = "arke";
  melinoe.node.networking.uplinks = [
    {
      ip = "130.95.13.236/32";
      pub_ip = "130.95.13.236/32";
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
  melinoe.services.melnode.extraRoutes = [ "130.95.13.0/24" ];
  # Kernel data plane (modules/melinoe-vpn-kernel) - RE-ENABLED after an
  # architecture rewrite (see modules/melinoe-vpn-kernel/ARCHITECTURE.md,
  # not tracked in git - local reference only). Root cause of the earlier
  # crash (tun-teardown control-channel stall, sleeping-lock/blocking-send
  # crash, and the __dev_queue_xmit recursion panic on tun-chaining) was a
  # single global spinlock making the whole datapath one undifferentiated
  # blob instead of a real forwarding design; the rewrite replaces it with
  # per-link locks, RCU-published route/link/tun tables, and - the actual
  # fix for the recursion panic - a bounded per-destination queue between
  # ingress and the forwarding decision (melnode_routing.c), so tun xmit
  # never again calls straight through to another tun's xmit inline.
  #
  # Still worth watching for on this first live pass: before the crash fix,
  # a real qdisc on the tuns turned the recursion panic into a silent
  # traffic-multiplication symptom instead (Tx counters hitting multi-GB
  # within ~2 minutes of six real peers coming up) - that was never
  # root-caused on its own, only worked around by disabling kernel mode
  # again. The new queue design should mean a hairpin shows up as two
  # independent, TTL-bounded queue items rather than unbounded duplication,
  # but that's exactly the scenario to specifically re-exercise (live
  # tcpdump on the node-* tuns during convergence) before trusting this on
  # anything that matters.
  melinoe.services.melnode.kernelDataplane.enable = true;
  melinoe.services.melnode.dataplane = "kernel";
  melinoe.node.networking.peers = [
    {
      id = 4;
    }
    {
      id = 2;
    }
    {
      id = 6;
      prependCount = 2;
    }
    {
      id = 7;
      prependCount = 2;
    }
    {
      id = 3;
    }
    {
      id = 1;
      prependCount = 1;
    }
  ];
}
