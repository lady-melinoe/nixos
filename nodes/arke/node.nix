{ config, pkgs, lib, inputs, modulesPath, ... }:

{
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
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

  boot.loader.grub = {
    efiSupport = true;
    configurationLimit = 20;
    efiInstallAsRemovable = true;
    device = "nodev";
  };
  boot.loader.efi.efiSysMountPoint = "/boot/efi";
  boot.initrd.availableKernelModules = [ "ata_piix" "uhci_hcd" "xen_blkfront" "vmw_pvscsi" ];
  boot.initrd.kernelModules = [ "nvme" ];

  networking.hostName = "arke";
  networking.domain = "";
  networking.useDHCP = false;
  networking.interfaces.ens18.useDHCP = false;
  networking.interfaces.ens19 = {
    useDHCP = false;
    ipv4.addresses = [{
      address = "198.19.0.${toString config.melinoe.nodeId}";
      prefixLength = 24;
    }];
  };

  systemd.oomd.enable = false;

  services.qemuGuest.enable = true;

  melinoe.inetIfs = [ "ens19" ];
  melinoe.nodeId = 2;
  melinoe.incusDefaultStorageSource = "/array/incus/";

  melinoe.wgPeers = [
    {
      id = 7;
      endpoint = "198.19.0.7";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
    {
      id = 3;
      endpoint = "198.19.0.3";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
    {
      id = 4;
      endpoint = "lachesis.infra.melinoe.xyz";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
    {
      id = 5;
      endpoint = "atropos.infra.melinoe.xyz";
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
      setupScript = pkgs.writeShellScript "melinoe-inet-setup" ''
        ip netns add inet 2>/dev/null
        ip link del inet0 2>/dev/null

        ip link add inet0 type veth peer name main
        ip link set main netns inet

        ip addr replace ${hostAddr}/32 dev inet0
        ip link set inet0 up

        ip netns exec inet ip addr replace 198.18.0.255/32 dev main
        ip netns exec inet ip link set main up

        ip netns exec inet ip route replace ${hostAddr}/32 dev main

        ip link set ens18 down 2>/dev/null 
        ip link set ens18 netns inet

        ip netns exec inet ip link set ens18 up
        ip netns exec inet ip addr flush dev ens18 
        ip netns exec inet ip addr replace 130.95.13.219/25 dev ens18

        ip netns exec inet iptables -t nat -C POSTROUTING -o ens18 -j MASQUERADE
        ip netns exec inet iptables -t nat -A POSTROUTING -o ens18 -j MASQUERADE
        
        ip route add 198.18.0.255 dev inet0
        ip route add default via 198.18.0.255 dev inet0

        ip netns exec inet ip route add default via 130.95.13.129 dev ens18 
        ip netns exec inet iptables -t nat -C PREROUTING -d 130.95.13.219 -j DNAT --to-destination 198.18.0.2
        ip netns exec inet iptables -t nat -A PREROUTING -d 130.95.13.219 -j DNAT --to-destination 198.18.0.2 
        ip netns exec inet sysctl -w net.ipv4.conf.default.rp_filter=0
        ip netns exec inet sysctl -w net.ipv4.conf.all.rp_filter=0
      '';
    in {
      description = "Configure inet netns veth pair for host<->inet connectivity";
      after = [ "network-pre.target" ];
      wants = [ "network-pre.target" ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = setupScript;
      };
      path = [ pkgs.iproute2 pkgs.iptables pkgs.procps ];
    };
}
