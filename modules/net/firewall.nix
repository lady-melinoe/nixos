{
  config,
  lib,
  melinoeLib,
  ...
}:
let
  inherit (lib) optionalString;
  cfg = config.melinoe.firewall;
  net = config.melinoe.net;
  cluster = config.melinoe.cluster;
  vms = lib.attrValues cluster.vms;

  ctMarkVia = 998;
  ctMarkUplink = 999;
  hairpinSnatAddress = "198.18.255.254";
  hiddenHopTtl = 62;

  quote = name: ''"${name}"'';
  set = items: "{ ${lib.concatStringsSep ", " items} }";
  portSet =
    ports: if lib.length ports == 1 then toString (lib.head ports) else set (map toString ports);

  indent = text: lib.concatMapStringsSep "\n" (line: "  ${line}") (lib.splitString "\n" text);
  block = header: lines: ''
    ${header} {
    ${indent (lib.concatStringsSep "\n" (lib.filter (line: line != "") lines))}
    }'';

  accessRules =
    {
      saddr ? null,
    }:
    access:
    let
      rule = condition: "${optionalString (saddr != null) "ip saddr ${saddr} "}${condition} accept";
    in
    lib.optional (access.tcp != [ ]) (rule "tcp dport ${portSet access.tcp}")
    ++ lib.optional (access.udp != [ ]) (rule "udp dport ${portSet access.udp}")
    ++ lib.optional (access.ipProtocols != [ ]) (rule "ip protocol ${portSet access.ipProtocols}");

  forwarded = lib.filter (vm: vm.tcp != [ ] || vm.udp != [ ]) vms;
  dnatRules =
    destination:
    lib.concatMap (
      vm:
      lib.optional (
        vm.tcp != [ ]
      ) "ip daddr ${destination} tcp dport ${portSet vm.tcp} dnat to ${vm.address}"
      ++ lib.optional (
        vm.udp != [ ]
      ) "ip daddr ${destination} udp dport ${portSet vm.udp} dnat to ${vm.address}"
    ) forwarded;

  hairpinRules =
    let
      pairs = set (map (vm: "${vm.address} . ${vm.address}") vms);
    in
    map
      (
        destination:
        "ct original ip daddr ${destination} ip saddr . ip daddr ${pairs} snat to ${hairpinSnatAddress}"
      )
      [
        "$hostaddr"
        "@pubroutefix"
      ];

  outboundRules = lib.concatMap (vm: [
    "iifname ${quote vm.iface} ct direction original meta mark set ${
      toString (net.outboundMarkBase + cluster.nodes.${vm.outboundVia}.id)
    }"
    "iifname ${quote vm.iface} ct direction original ct mark set ${toString ctMarkVia}"
  ]) (lib.filter (vm: vm.outboundVia != null) vms);

  uplinkMark = toString net.uplinkMark;
in
{
  options.melinoe.firewall = {
    openPorts = melinoeLib.mkAccessRuleOption "Accepted from anywhere.";
    nodePorts = melinoeLib.mkAccessRuleOption "Accepted from the node address range.";
    meshPorts = melinoeLib.mkAccessRuleOption "Accepted from anywhere in the mesh.";
  };

  config = {
    networking = {
      firewall.enable = false;
      nftables.enable = true;
      nftables.ruleset = lib.concatStringsSep "\n" [
        "flush ruleset"
        "define uplink_ifs = ${set (map quote net.uplinkNames)}"
        "define hostaddr = ${config.melinoe.node.address}"

        (block "table inet filter" [
          (block "chain INPUT" (
            [
              "type filter hook input priority filter; policy drop;"
              "ct state invalid drop"
              "ct state { established, related } accept"
              "icmp type { echo-request, echo-reply } accept"
              "icmpv6 type { echo-request, nd-neighbor-solicit } accept"
              ''iif "lo" accept''
            ]
            ++ accessRules { } cfg.openPorts
            ++ accessRules { saddr = set [ cluster.networks.mesh ]; } cfg.meshPorts
            ++ accessRules { saddr = cluster.networks.nodes; } cfg.nodePorts
            ++ lib.concatMap (vm: accessRules { saddr = vm.source; } vm.hostAccess) vms
          ))
          (block "chain FORWARD" (
            [
              "type filter hook forward priority filter; policy accept;"
              "ct state invalid drop"
            ]
            ++ lib.concatMap (vm: map (ip: "ip saddr ${vm.source} ip daddr ${ip} drop") vm.outboundDropIP) vms
            ++ lib.concatMap (
              vm:
              lib.optionals (vm.outboundAllowOnlyIP != [ ]) (
                map (ip: "ip saddr ${vm.source} ip daddr ${ip} accept") vm.outboundAllowOnlyIP
                ++ [ "ip saddr ${vm.source} drop" ]
              )
            ) vms
            ++ [
              "ct state { established, related } accept"
              "icmp type { echo-request, echo-reply } accept"
              "icmpv6 type { echo-request, nd-neighbor-solicit } accept"
            ]
          ))
          (block "chain OUTPUT" [ "type filter hook output priority filter; policy accept;" ])
        ])

        (block "table inet raw" [
          (block "chain prerouting" (
            [
              "type filter hook prerouting priority raw; policy accept;"
              "iifname $uplink_ifs ip saddr ${cluster.networks.mesh} drop"
            ]
            ++ map (vm: "iifname ${quote vm.iface} ip saddr != ${vm.source} drop") vms
          ))
        ])

        (block "table ip nat" [
          (block "set pubroutefix" (
            [
              "type ipv4_addr"
              "flags interval"
            ]
            ++ lib.optional (net.publicPrefixes != [ ]) "elements = ${set net.publicPrefixes}"
          ))
          (block "chain prerouting" (
            [ "type nat hook prerouting priority dstnat;" ] ++ dnatRules "$hostaddr" ++ dnatRules "@pubroutefix"
          ))
          (block "chain postrouting" (
            [ "type nat hook postrouting priority srcnat;" ]
            ++ hairpinRules
            ++ map (name: "oifname ${quote name} masquerade") net.uplinkNames
          ))
          (block "chain output" [ "type nat hook output priority dstnat; policy accept;" ])
        ])

        (block "table inet mangle" [
          (block "set melinoe_peer_marks" [ "type mark" ])
          (block "map melinoe_peer_ifaces" [ "type ifname : mark" ])
          (block "chain prerouting" (
            [ "type filter hook prerouting priority mangle;" ]
            ++ outboundRules
            ++ [
              "iifname != $uplink_ifs ct direction reply ct mark ${toString ctMarkUplink} meta mark set ${uplinkMark}"
              "iifname $uplink_ifs ct direction original ct mark != ${toString ctMarkUplink} ct mark set ${toString ctMarkUplink}"
              "ct direction reply ct mark @melinoe_peer_marks meta mark set ct mark"
              "ct mark != ${toString ctMarkVia} ct direction original ct mark != @melinoe_peer_marks ct mark set iifname map @melinoe_peer_ifaces"
            ]
          ))
          (block "chain output" [
            "type route hook output priority mangle;"
            "ct direction reply ct mark ${toString ctMarkUplink} meta mark set ${uplinkMark}"
            "ct direction reply ct mark @melinoe_peer_marks meta mark set ct mark"
            "ct mark ${toString ctMarkVia} ip ttl set ${toString hiddenHopTtl}"
          ])
        ])
      ];
    };
  };
}
