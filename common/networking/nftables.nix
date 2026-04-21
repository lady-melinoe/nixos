{ config, lib, ... }:
let
  cfg = config.melinoe;
  hostAddr = "198.18.0.${toString cfg.nodeId}";
  pubRouteFix = lib.filter (ip: ip != null) (map (entry: entry.pub_ip or null) cfg.internet);
  natMappings = [
    { dst = "198.18.1.5"; tcp = [ 8080 ]; udp = [ 8080 ]; }
    { dst = "198.18.1.1"; tcp = [ 80 443 1080 1443 ]; udp = [ 80 443 1080 1443 ]; }
    { dst = "198.18.1.6"; tcp = [ 993 25 465 ]; udp = [ ]; }
    { dst = "198.18.1.8"; tcp = [ 25565 ]; udp = [ ]; }
    { dst = "198.18.1.11"; tcp = [ 57843 ]; udp = [ 57843 ]; }
    { dst = "198.18.3.0"; tcp = [ ]; udp = [ 51820 ]; }
  ];
  portSet = ports:
    if ports == [ ] then ""
    else if builtins.length ports == 1 then toString (builtins.head ports)
    else "{ ${builtins.concatStringsSep ", " (map toString ports)} }";
  renderProtoRule = proto: ports: matchExpr: dst:
    lib.optionalString (ports != [ ]) ''
        ${matchExpr} ${proto} dport ${portSet ports} dnat to ${dst}
    '';
  renderIfaceRules = ifaceExpr:
    lib.concatMapStrings (mapping:
      (renderProtoRule "tcp" mapping.tcp "iifname ${ifaceExpr}" mapping.dst)
      + (renderProtoRule "udp" mapping.udp "iifname ${ifaceExpr}" mapping.dst)
    ) natMappings;
  renderDestRules = destExpr:
    lib.concatMapStrings (mapping:
      (renderProtoRule "tcp" mapping.tcp "ip daddr ${destExpr}" mapping.dst)
      + (renderProtoRule "udp" mapping.udp "ip daddr ${destExpr}" mapping.dst)
    ) natMappings;
in {
  networking.firewall.enable = false;
  networking.nftables.enable = true;
  networking.nftables.ruleset = ''
    flush ruleset
    define vm_ifs = "vm-*"
    define node_gre_ifs = "node-*"
    define wg_ifs = "wg-*"
    define gre_ctmark = { ${builtins.concatStringsSep ", " (builtins.genList (i: "\"node-${toString (i)}\" : ${toString (1000 + i)}") 255)} }
${lib.optionalString (pubRouteFix != [ ]) ''
    define pubroutefix = { ${builtins.concatStringsSep ", " pubRouteFix} }
''}
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
        tcp dport 2049 accept
        udp dport 2049 accept
        tcp dport 4269 accept
        udp dport 4269 accept
        udp dport 6969 accept
        ip saddr 198.18.0.0/15 tcp dport 5201 accept # iperf3
        iifname $wg_ifs ip saddr 198.19.3.0/24 tcp dport 179 accept
        iifname $wg_ifs ip saddr 198.19.3.0/24 udp dport { 3784, 3785 } accept
        iifname $wg_ifs ip saddr 198.19.3.0/24 udp sport { 3784, 3785 } accept
        iifname $wg_ifs ip saddr 198.51.100.0/24 ip protocol gre accept
        ip saddr 198.18.0.0/24 tcp dport 60198 accept
        ip saddr 198.18.1.5 tcp dport 61208 accept
${lib.optionalString (cfg.wgPorts != [ ]) ''
        udp dport { ${builtins.concatStringsSep ", " (map toString cfg.wgPorts)} } accept
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
        iifname $vm_ifs ip saddr 198.18.0.0/24 drop
        iifname $vm_ifs ip saddr != 198.18.0.0/16 drop
      }
    }

    table ip nat {
      chain prerouting {
        type nat hook prerouting priority dstnat;
${renderIfaceRules "\"inet0\""}
${lib.optionalString (pubRouteFix != [ ]) (renderDestRules "$pubroutefix")}
${lib.optionalString (pubRouteFix != [ ]) ''
        ip daddr $pubroutefix dnat to ${hostAddr}
''}
      }
      chain postrouting {
        type nat hook postrouting priority srcnat;
        oifname "inet0" masquerade
      }
      chain output {
        type nat hook output priority dstnat; policy accept;
${lib.optionalString (pubRouteFix != [ ]) (renderDestRules "$pubroutefix")}
${lib.optionalString (pubRouteFix != [ ]) ''
        ip daddr $pubroutefix dnat to ${hostAddr}
''}
      }
    }

    table inet mangle {
      chain prerouting {
        type filter hook prerouting priority mangle;
        iifname $vm_ifs ct direction reply ct mark 1000-1254 meta mark set ct mark
        iifname $node_gre_ifs ct direction original ct mark != 1000-1254 ct mark set iifname map $gre_ctmark
        iifname inet0 ct direction reply ct mark 999 meta mark set 51820
        iifname inet0 ct direction original ct mark != 999 ct mark set 999
      }
      chain output {
        type route hook output priority mangle;
        ct direction reply ct mark 1000-1254 meta mark set ct mark
        ct direction reply ct mark 999 meta mark set 51820
      }
    }
  '';
}
