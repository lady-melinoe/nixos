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
    enable = true;
    configurationLimit = 20;
  };

  boot.initrd.availableKernelModules = [ "ata_piix" "uhci_hcd" "xen_blkfront" "vmw_pvscsi" ];
  boot.initrd.kernelModules = [ "nvme" ];

  networking.hostName = "phaesyle";
  networking.domain = "";
  networking.useDHCP = false;
  networking.interfaces.ens3.useDHCP = false;

  systemd.oomd.enable = false;

  services.qemuGuest.enable = true;

  melinoe.inetIfs = [ "ens99" ];
  melinoe.nodeId = 8;
  melinoe.incusDefaultStorageSource = "/array/incus/";

  melinoe.wgPeers = [
    {
      id = 7;
      endpoint = "benzaiten.infra.melinoe.xyz";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
    {
      id = 2;
      endpoint = "arke.infra.melinoe.xyz";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
    {
      id = 6;
      endpoint = "ceridwen.infra.melinoe.xyz";
      allowedIPs = [ "198.19.3.0/24" "198.51.100.0/24" ];
    }
    {
      id = 3;
      endpoint = "hecate.infra.melinoe.xyz";
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

        ip link set ens3 down 2>/dev/null 
        ip link set ens3 netns inet

        ip netns exec inet ip link set ens3 up
        ip netns exec inet ip addr flush dev ens3 
        ip netns exec inet ip addr replace 103.249.239.233/24 dev ens3

        ip netns exec inet iptables -t nat -C POSTROUTING -o ens3 -j MASQUERADE
        ip netns exec inet iptables -t nat -A POSTROUTING -o ens3 -j MASQUERADE
        
        ip route add 198.18.0.255 dev inet0
        ip route add default via 198.18.0.255 dev inet0

        ip netns exec inet ip route add default via 103.249.239.1 dev ens3 

        ip netns exec inet iptables -t nat -C PREROUTING -d 103.249.239.233 -j DNAT --to-destination 198.18.0.8
        ip netns exec inet iptables -t nat -A PREROUTING -d 103.249.239.233 -j DNAT --to-destination 198.18.0.8 
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
