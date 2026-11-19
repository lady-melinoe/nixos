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

  portfwdElements = lib.concatMap (
    vm:
    map (port: "tcp . ${toString port} : ${vm.address}") vm.tcp
    ++ map (port: "udp . ${toString port} : ${vm.address}") vm.udp
  ) vms;

  outboundElements = map (
    vm: "${quote vm.iface} : ${toString (net.outboundMarkBase + cluster.nodes.${vm.outboundVia}.id)}"
  ) (lib.filter (vm: vm.outboundVia != null) vms);

  object = kind: name: lines: {
    inherit kind name lines;
    persistent = false;
  };
  persistentObject =
    kind: name: lines:
    object kind name lines // { persistent = true; };
  chain = object "chain";
  table = name: objects: { inherit name objects; };

  renderTable = t: block "table ${t.name}" (map (o: block "${o.kind} ${o.name}" o.lines) t.objects);

  tableDeletions =
    t:
    let
      managed = lib.filter (o: !o.persistent) t.objects;
      chains = lib.filter (o: o.kind == "chain") managed;
      others = lib.filter (o: o.kind != "chain") managed;
    in
    if lib.all (o: !o.persistent) t.objects then
      [ "destroy table ${t.name}" ]
    else
      [ "table ${t.name}" ] ++ map (o: "destroy ${o.kind} ${t.name} ${o.name}") (chains ++ others);

  uplinkMark = toString net.uplinkMark;
  localOutboundMark = toString (net.outboundMarkBase + config.melinoe.node.id);

  tables = [
    (table "inet filter" [
      (chain "INPUT" (
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
      (chain "FORWARD" (
        [
          "type filter hook forward priority filter; policy accept;"
          "ct state invalid drop"
          "iifname ${quote net.meshInterface} tcp flags syn tcp option maxseg size set rt mtu"
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
      (chain "OUTPUT" [ "type filter hook output priority filter; policy accept;" ])
    ])

    (table "inet raw" [
      (chain "prerouting" ([
        "type filter hook prerouting priority raw; policy accept;"
        "iifname $uplink_ifs ip saddr ${cluster.networks.mesh} drop"
      ]))
    ])

    (table "ip nat" [
      (object "set" "pubroutefix" (
        [
          "type ipv4_addr"
          "flags interval"
        ]
        ++ lib.optional (net.publicPrefixes != [ ]) "elements = ${set net.publicPrefixes}"
      ))
      (object "map" "portfwd" (
        [ "typeof meta l4proto . th dport : ip daddr" ]
        ++ lib.optional (portfwdElements != [ ]) "elements = ${set portfwdElements}"
      ))
      (chain "prerouting" ([
        "type nat hook prerouting priority dstnat;"
        "ip daddr $hostaddr dnat to meta l4proto . th dport map @portfwd"
        "ip daddr @pubroutefix dnat to meta l4proto . th dport map @portfwd"
      ]))
      (chain "postrouting" (
        [
          "type nat hook postrouting priority srcnat;"
          "ct status dnat ip saddr 198.18.1.0-198.18.255.253 snat ip to ip saddr ^ ip daddr map { 0.0.0.0 : ${hairpinSnatAddress} }"
        ]
        ++ map (name: "oifname ${quote name} masquerade") net.uplinkNames
      ))
      (chain "output" [
        "type nat hook output priority dstnat; policy accept;"
        "ip daddr $hostaddr dnat to meta l4proto . th dport map @portfwd"
        "ip daddr @pubroutefix dnat to meta l4proto . th dport map @portfwd"
      ])
    ])

    (table "inet mangle" [
      (persistentObject "set" "melinoe_peer_marks" [ "type mark" ])
      # Sender node id (the link-layer source address on the mesh device) -> its mark.
      (persistentObject "map" "melinoe_peer_ids" [ "typeof @ll,8,8 : mark" ])
      (object "map" "outbound_via" (
        [ "type ifname : mark" ]
        ++ lib.optional (outboundElements != [ ]) "elements = ${set outboundElements}"
      ))
      (chain "prerouting" (
        [ "type filter hook prerouting priority mangle;" ]
        ++ [
          "ct direction original meta mark set iifname map @outbound_via ct mark set ${toString ctMarkVia}"
          "iifname != $uplink_ifs ct direction reply ct mark ${toString ctMarkUplink} meta mark set ${uplinkMark}"
          "iifname $uplink_ifs ct direction original ct mark != ${toString ctMarkUplink} ct mark set ${toString ctMarkUplink}"
          "ct direction reply ct mark @melinoe_peer_marks meta mark set ct mark"
          "iifname ${quote net.meshInterface} ct mark != ${toString ctMarkVia} ct direction original ct mark != @melinoe_peer_marks ct mark set @ll,8,8 map @melinoe_peer_ids"
        ]
      ))
      (chain "output" [
        "type route hook output priority mangle;"
        "ct direction reply ct mark ${toString ctMarkUplink} meta mark set ${uplinkMark}"
        "ct direction reply ct mark @melinoe_peer_marks meta mark set ct mark"
        "ct mark ${toString ctMarkVia} meta mark != ${localOutboundMark} ip ttl set ${toString hiddenHopTtl}"
      ])
    ])
  ];
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
      nftables.checkRuleset = false;
      nftables.flushRuleset = false;
      nftables.extraDeletions = lib.concatStringsSep "\n" (lib.concatMap tableDeletions tables);
      nftables.ruleset = lib.concatStringsSep "\n" (
        [
          "define uplink_ifs = ${set (map quote net.uplinkNames)}"
          "define hostaddr = ${config.melinoe.node.address}"
        ]
        ++ map renderTable tables
      );
    };
  };
}
