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
  # Kernel data plane testing (modules/melinoe-vpn-kernel) - DISABLED again
  # after a live session on arke found (and fixed) a tun-teardown control-
  # channel stall and a sleeping-lock/blocking-send crash, then exposed a
  # separate, still-unresolved bug: real IP forwarding chained between two
  # node-* tuns loops (previously crashed the box outright via
  # __dev_queue_xmit's recursion guard; now that the tuns get a real qdisc
  # instead of IFF_NO_QUEUE, it no longer crashes but silently multiplies
  # traffic - confirmed live, Tx counters hitting multi-GB within ~2
  # minutes of six real peers coming up). Not safe to leave enabled until
  # that forwarding loop itself is root-caused (needs a live tcpdump on the
  # node-* tuns while it's happening) - melnode-cp goes back to talking to
  # melnode-dp (userspace) in the meantime.
  melinoe.services.melnode.kernelDataplane.enable = false;
  melinoe.services.melnode.dataplane = "userspace";
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
