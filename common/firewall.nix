{ config, lib, ... }:

{

  networking.firewall.enable = false;
  networking.nftables.enable = true;
  networking.nftables.ruleset = ''
    flush ruleset
    define inet_ifs = { ${lib.concatStringsSep ", " (map (i: "\"${i}\"") config.melinoe.inetIfs)} }
    define vm_ifs = "vm-*"
    define node_gre_ifs = "node-*"
    define wg_ifs = "wg-*"
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
        tcp dport 8008 accept
	tcp dport 4269 accept
        udp dport 4269 accept
        iifname $wg_ifs ip saddr 198.19.3.0/24 tcp dport 179 accept
        iifname $wg_ifs ip saddr 198.51.100.0/24 ip protocol gre accept
        ip saddr 198.18.0.0/24 tcp dport 60198 accept
        ip saddr 198.18.1.5 tcp dport 61208 accept
${lib.optionalString (config.melinoe.wgPorts != [ ]) ''
        udp dport { ${builtins.concatStringsSep ", " (map toString config.melinoe.wgPorts)} } accept
''}
      }

      chain FORWARD {
        type filter hook forward priority filter; policy accept;
        ct state invalid drop
        ip saddr 198.18.1.13 ip daddr 35.190.167.255 drop
        ct state { established, related } accept
        icmp type { echo-request, echo-reply } accept
        icmpv6 type { echo-request, nd-neighbor-solicit } accept
      }

      chain OUTPUT {
        type filter hook output priority filter; policy accept;
      }

    }

    table inet raw {
      chain prerouting {
        type filter hook prerouting priority raw; policy accept;
        iifname "node-8" tcp dport 8080 meta nftrace set 1
        iifname "node-8" tcp sport 8080 meta nftrace set 1
        iifname $inet_ifs ip saddr { 198.18.0.0/16, 198.51.100.0/24 } drop
        iifname $inet_ifs ip daddr { 198.18.0.0/16, 198.51.100.0/24 } drop
        iifname $vm_ifs ip saddr 198.18.0.0/24 drop
        iifname $vm_ifs ip saddr != 198.18.0.0/16 drop 
      }
    }

    table ip nat {
      chain prerouting {
        type nat hook prerouting priority dstnat;
        iifname $inet_ifs tcp dport 8080 dnat to 198.18.1.5
        iifname $inet_ifs udp dport 8080 dnat to 198.18.1.5
        iifname $inet_ifs tcp dport { 80, 443 } dnat to 198.18.1.1
        iifname $inet_ifs udp dport { 80, 443 } dnat to 198.18.1.1
        iifname $inet_ifs tcp dport { 993, 25, 465 } dnat to 198.18.1.6
        iifname $inet_ifs tcp dport { 25565 } dnat to 198.18.1.8
        iifname $inet_ifs tcp dport { 57843 } dnat to 198.18.1.11
        iifname $inet_ifs udp dport { 57843 } dnat to 198.18.1.11

      }
      chain postrouting {
        type nat hook postrouting priority srcnat;
        ip saddr 198.18.0.0/16 oifname $inet_ifs masquerade
      }
    }

    table inet mangle {
      chain prerouting {
        type filter hook prerouting priority mangle;
        iifname $vm_ifs ct direction reply ct mark 1000-1254 meta mark set ct mark
        iifname $node_gre_ifs ct direction original ct mark != 1000-1254 ct mark set iifname map $gre_ctmark
      }
      chain output {
        type route hook output priority mangle;
        ct direction reply ct mark 1000-1254 meta mark set ct mark
      }
    }
  '';
}
