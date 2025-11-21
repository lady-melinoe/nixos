{ config, lib, pkgs, ... }:

let
  nodeID = 7;
  configureIface = pkgs.writeShellScriptBin "configure-iface" ''
    #!/usr/bin/env bash
    IFACE="$1"
    ACTION="$2"
    IP="$3"
    RES_FILE="/etc/melinoe/residents/list"

    if [ -z "$IFACE" ] || [ -z "$ACTION" ] || [ -z "$IP" ]; then
      echo "Usage: $0 <iface> <up|down> <ip>"
      exit 1
    fi

    mkdir -p "$(dirname "$RES_FILE")"
    touch "$RES_FILE"

    case "$ACTION" in
      up)
        echo 1 > /proc/sys/net/ipv4/conf/$IFACE/forwarding
        echo 1 > /proc/sys/net/ipv4/conf/$IFACE/proxy_arp
        ip addr replace 198.18.0.7/32 dev "$IFACE"

        if ! grep -Fxq "$IP" "$RES_FILE"; then
          echo "$IP" >> "$RES_FILE"
        fi
        ;;
      down)
        sed -i "\|^$IP\$|d" "$RES_FILE"
        ;;
      *)
        echo "Invalid action: $ACTION"
        echo "Use: up or down"
        exit 1
        ;;
    esac
  '';
in
{
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
      address = "198.19.0.${toString nodeID}";
      prefixLength = 24;
    }];
  };

  networking.interfaces.lo.ipv4.addresses = [{
    address = "198.51.100.${toString nodeID}";
    prefixLength = 32;
  } {
    address = "198.18.0.${toString nodeID}";
    prefixLength = 32;
  }];

  services.frr = {
    bgpd.enable = true;
    config = ''
      ip prefix-list NODE-LOOPS permit 198.51.100.0/24 le 32

      route-map NODE-IN permit 10
       match ip address prefix-list NODE-LOOPS

      route-map NODE-OUT permit 10
       match ip address prefix-list NODE-LOOPS

      router bgp ${toString (64512 + nodeID)}
        bgp router-id 198.51.100.${toString nodeID}

        neighbor 198.19.0.2 remote-as ${toString (64512 + 2)}
        neighbor 198.19.0.2 update-source 198.19.0.${toString nodeID}
        neighbor 198.19.0.3 remote-as ${toString (64512 + 3)}
        neighbor 198.19.0.3 update-source 198.19.0.${toString nodeID}
        neighbor 198.19.0.6 remote-as ${toString (64512 + 6)}
        neighbor 198.19.0.6 update-source 198.19.0.${toString nodeID}

        address-family ipv4 unicast
          network 198.51.100.${toString nodeID}/32
          neighbor 198.19.0.2 activate
          neighbor 198.19.0.2 route-map NODE-IN in
          neighbor 198.19.0.2 route-map NODE-OUT out
          neighbor 198.19.0.3 activate
          neighbor 198.19.0.3 route-map NODE-IN in
          neighbor 198.19.0.3 route-map NODE-OUT out
          neighbor 198.19.0.6 activate
          neighbor 198.19.0.6 route-map NODE-IN in
          neighbor 198.19.0.6 route-map NODE-OUT out
        exit-address-family
      !
    '';
  };

  networking.firewall.enable = false;
  networking.nftables.enable = true;
  networking.nftables.ruleset = ''
    flush ruleset
    define inet_ifs = "eno1"
    define p2p_ifs = { "eno3", "eno4", "bond0" }
    define vm_ifs = "vm-*"
    define node_ifs = "node-*"
    define private_vlan = 198.18.0.0/15
    define gre_ctmark = { ${builtins.concatStringsSep ", " (builtins.genList (i: "\"node-${toString (i)}\" : ${toString (1000 + i)}") 255)} }

    table inet filter {
      chain INPUT {
        type filter hook input priority filter; policy drop;
        ct state invalid drop
        ct state { established, related } accept
        icmp type { echo-request, echo-reply } accept
        icmpv6 type { echo-request, nd-neighbor-solicit } accept
        iif "lo" accept
        tcp dport 22 accept
        iifname $p2p_ifs ip protocol gre accept
        iifname $p2p_ifs tcp dport 179 accept
        iifname $node_ifs tcp dport 60198 accept
        ip saddr 198.18.1.3 tcp dport 61208 accept
        tcp dport 8008 accept
      }

      chain FORWARD {
        type filter hook forward priority filter; policy accept;
        ct state invalid drop
        ct state { established, related } accept
        icmp type { echo-request, echo-reply } accept
        icmpv6 type { echo-request, nd-neighbor-solicit } accept
      }

      chain OUTPUT {
        type filter hook output priority filter; policy accept;
      }

    }

    table ip nat {

      chain prerouting {
        type nat hook prerouting priority dstnat; policy accept;
        iifname $inet_ifs tcp dport { 80, 443 } dnat to 198.18.1.1
        iifname $inet_ifs tcp dport { 993, 25, 465 } dnat to 198.18.1.6
      }

      chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        oifname $inet_ifs masquerade
      }
    }

    table inet mangle {
      chain prerouting {
        type filter hook prerouting priority mangle; policy accept;
        iifname != $node_ifs ct direction reply ct mark 1000-1254 meta mark set ct mark
        iifname $node_ifs ct direction original ct mark != 1000-1254 ct mark set iifname map $gre_ctmark
      }

      chain output {
        type route hook output priority mangle; policy accept;
        iifname != $node_ifs ct direction reply ct mark 1000-1254 meta mark set ct mark
      }
    }
  '';
  virtualisation.incus.enable = true;
  users.users.melinoe.extraGroups = [ "incus-admin" ];
  virtualisation.incus.preseed = {
    networks = [];
    profiles = [
      {
        devices = {
          root = {
            path = "/";
            pool = "default";
            size = "35GiB";
            type = "disk";
          };
        };
        name = "default";
      }
    ];
    storage_pools = [
      {
        config = {
          source = "/var/lib/incus/storage-pools/default";         };
        driver = "btrfs";
        name = "default";
      }
    ];
  };
  system.activationScripts.incusConfigureIface = {
    text = ''
      mkdir -p /etc/incus/hooks
      ln -sf ${configureIface}/bin/configure-iface /etc/incus/hooks/configure-iface
    '';
  };

  services.glances.enable = true;
  services.glances.port = 61208;
  services.glances.extraArgs = [ "--webserver" "-B" "198.18.0.${toString nodeID}" ];
  services.static-web-server = {
    enable = true;
    listen = "198.18.0.${toString nodeID}:60198";
    root = "/etc/melinoe/residents/";
  };

  systemd.services.melinoe-route-deploy = {
    description = "Run melinoe route deployment";
    after = [ "network-online.target" ];
    wants = [ "network-online.target" ];
    path = [
      pkgs.coreutils
      pkgs.bash
      pkgs.iproute2
      pkgs.frr
      pkgs.jq
      pkgs.curl
      pkgs.gnugrep
      pkgs.gawk
      pkgs.gnused
      pkgs.util-linux
    ];
    serviceConfig = {
      Type = "oneshot";
      ConditionPathIsExecutable = "/etc/nixos/route-deploy.sh";
      ExecStart = "${pkgs.coreutils}/bin/timeout 30 /etc/nixos/route-deploy.sh";
    };
  };

  systemd.timers.melinoe-route-deploy = {
    wantedBy = [ "timers.target" ];
    timerConfig = {
      OnBootSec = "3s";
      OnUnitInactiveSec = "3s";
      AccuracySec = "1s";
    };
  };
}
