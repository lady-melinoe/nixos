{ config, lib, pkgs, ... }:

let
  nodeID = config.melinoe.nodeId;
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
        ip saddr 198.18.1.5 tcp dport 61208 accept
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
}
