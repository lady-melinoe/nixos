{ config, lib, pkgs, ... }:
{

  networking.firewall.enable = false;
  networking.nftables.enable = true;
  networking.nftables.ruleset = ''
    flush ruleset
    define inet_ifs = "ens18"
    define p2p_ifs = "ens19"
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
        iifname $vm_ifs ip saddr 198.18.0.0/24 drop
        iifname $vm_ifs ip saddr 198.51.100.0/24 drop
        iifname $vm_ifs ip saddr != 198.18.0.0/16 drop 
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
        ct direction reply ct mark 1000-1254 meta mark set ct mark
        iifname $node_ifs ct direction original ct mark != 1000-1254 ct mark set iifname map $gre_ctmark
      }

      chain output {
        type route hook output priority mangle; policy accept;
        ct direction reply ct mark 1000-1254 meta mark set ct mark
      }
    }
  '';

}
