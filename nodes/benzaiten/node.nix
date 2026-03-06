{ config, pkgs, lib, inputs, modulesPath, ... }:

{

  imports = [
    inputs.disko.nixosModules.disko
    ../../common/node-opts.nix
    ../../common/overlay.nix
    ../../common/underlay.nix
    ../../common/settings.nix
    ../../common/firewall.nix
    ../../common/users.nix
    ../../common/container-backup.nix
    ../../common/wireguard.nix
    ./disk-config.nix
  ];

  networking.hostName = "benzaiten";
  networking.domain = "";
  networking.useDHCP = false;
  networking.interfaces.eno1.useDHCP = true;
  networking.interfaces.eno3.useDHCP = false;
  networking.interfaces.eno4.useDHCP = false;
  networking.bonds.bond0 = {
    interfaces = [ "eno3" "eno4" ];
    driverOptions = {
      mode = "802.3ad";
      lacp_rate = "fast";
    };
  };
  networking.interfaces.bond0 = {
    useDHCP = false;
    ipv4.addresses = [{
      address = "198.19.0.${toString config.melinoe.nodeId}";
      prefixLength = 24;
    }];
  };

  boot.loader.grub = {
    efiSupport = true;
    configurationLimit = 20;
    efiInstallAsRemovable = true;
    device = "nodev";
  };

  boot.loader.efi.efiSysMountPoint = "/boot/efi";
  boot.initrd.availableKernelModules = [ "sd_mod" "usbhid" "usb_storage" "mpt3sas" "ehci_pci" ];
  boot.initrd.kernelModules = [ "kvm-intel" ];

  melinoe.inetIfs = [ "eno1" "bond0" ];
  melinoe.nodeId = 7;
  melinoe.incusDefaultStorageSource = "/array/incus/";

  melinoe.wgPeers = [
    {
      id = 5;
      endpoint = "atropos.infra.melinoe.xyz";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
    {
      id = 2;
      endpoint = "198.19.0.2";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
    {
      id = 4;
      endpoint = "lachesis.infra.melinoe.xyz";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
    {
      id = 6;
      endpoint = "198.19.0.6";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
    {
      id = 8;
      endpoint = "phaesyle.infra.melinoe.xyz";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
  ];

  systemd.services.melinoe-inet-setup =
    let
      hostAddr = "198.18.0.${toString config.melinoe.nodeId}";
      netnsAddr = "198.18.0.255";
      bondAddr = "198.19.0.${toString config.melinoe.nodeId}/24";
      setupScript = pkgs.writeShellScript "melinoe-inet-setup" ''
        set -euo pipefail

        NETNS=inet
        SLAVES=(eno3 eno4)

        ip netns add inet 2>/dev/null || true
        ip link del inet0 2>/dev/null || true

        ip link add inet0 type veth peer name main
        ip link set main netns "$NETNS"

        ip addr replace ${hostAddr}/32 dev inet0
        ip link set inet0 up

        ip netns exec "$NETNS" ip addr replace ${netnsAddr}/32 dev main
        ip netns exec "$NETNS" ip link set main up

        ip route replace ${netnsAddr}/32 dev inet0
        ip netns exec "$NETNS" ip route replace ${hostAddr}/32 dev main

        # Move bond slaves into the netns and rebuild bond0 inside it
        for iface in "''${SLAVES[@]}"; do
          ip link set "$iface" down 2>/dev/null || true
          ip link set "$iface" nomaster 2>/dev/null || true
          ip link set "$iface" netns "$NETNS"
        done

        ip link set bond0 down 2>/dev/null || true
        ip link del bond0 2>/dev/null || true
        ip netns exec "$NETNS" ip link set bond0 down 2>/dev/null || true
        ip netns exec "$NETNS" ip link del bond0 2>/dev/null || true

        ip netns exec "$NETNS" ip link add bond0 type bond mode 802.3ad
        ip netns exec "$NETNS" ip link set bond0 type bond lacp_rate 1

        for iface in "''${SLAVES[@]}"; do
          ip netns exec "$NETNS" ip link set "$iface" down 2>/dev/null || true
          ip netns exec "$NETNS" ip addr flush dev "$iface" || true
          ip netns exec "$NETNS" ip link set "$iface" master bond0
        done

        ip netns exec "$NETNS" ip link set bond0 up
        ip netns exec "$NETNS" ip addr replace ${bondAddr} dev bond0
        ip netns exec "$NETNS" iptables -t nat -C POSTROUTING -o bond0 -j MASQUERADE 2>/dev/null || ip netns exec "$NETNS" iptables -t nat -A POSTROUTING -o bond0 -j MASQUERADE
      '';
    in {
      description = "Configure inet netns veth pair for host<->inet connectivity";
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = setupScript;
      };
      path = [ pkgs.iproute2 pkgs.iptables ];
    };
}
