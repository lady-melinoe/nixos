{
  config,
  lib,
  melinoeNodeIntraIP,
  ...
}:
let
  netCfg = config.melinoe.node.networking;
  addr = config.melinoe.cluster.networking;
  nodeID = config.melinoe.node.id;
  hostAddr = melinoeNodeIntraIP nodeID;
  pubIps = lib.filter (ip: ip != null) (map (entry: entry.pub_ip or null) netCfg.uplinks);
  uplinkIface =
    idx: uplink:
    if builtins.length uplink.iface > 1 then "bond${toString idx}" else builtins.head uplink.iface;
  uplinkIpAddr = uplink: builtins.head (lib.splitString "/" uplink.ip);
  uplinkIfaceNames = lib.imap0 uplinkIface netCfg.uplinks;
  renderUplinkDnatRules = lib.concatStringsSep "\n" (
    lib.imap0 (
      idx: uplink:
      ''iifname "${uplinkIface idx uplink}" ip daddr ${uplinkIpAddr uplink} dnat to ${hostAddr}''
    ) netCfg.uplinks
  );
  renderUplinkMasqueradeRules = lib.concatStringsSep "\n" (
    map (iface: ''oifname "${iface}" masquerade'') uplinkIfaceNames
  );
  vms = config.melinoe.cluster.virtualMachines;
  vmIpAddr = vm: builtins.head (lib.splitString "/" vm.ip);
  vmSaddr = vm: if lib.hasInfix "/" vm.ip then vm.ip else "${vm.ip}/32";
  vmTcp = vm: vm.tcp or [ ];
  vmUdp = vm: vm.udp or [ ];
  natMappings = lib.filter (m: m.tcp != [ ] || m.udp != [ ]) (
    map (vm: {
      dst = vmIpAddr vm;
      tcp = vmTcp vm;
      udp = vmUdp vm;
    }) vms
  );
  renderVmSaddrRules = lib.concatMapStrings (vm: ''
    iifname "${vm.iface}" ip saddr != ${vmSaddr vm} drop
  '') vms;
  portSet =
    items:
    if items == [ ] then
      ""
    else if builtins.length items == 1 then
      toString (builtins.head items)
    else
      "{ ${builtins.concatStringsSep ", " (map toString items)} }";
  renderProtoRule =
    proto: ports: matchExpr: dst:
    lib.optionalString (ports != [ ]) ''
      ${matchExpr} ${proto} dport ${portSet ports} dnat to ${dst}
    '';
  renderDestRules =
    destExpr:
    lib.concatMapStrings (
      mapping:
      (renderProtoRule "tcp" mapping.tcp "ip daddr ${destExpr}" mapping.dst)
      + (renderProtoRule "udp" mapping.udp "ip daddr ${destExpr}" mapping.dst)
    ) natMappings;
  nftIfaceSet =
    names: if names == [ ] then "{ }" else "{ ${lib.concatStringsSep ", " (map (n: "\"${n}\"") names)} }";
  wgIfaceNames = map (peer: "wg-${toString peer.id}") netCfg.peers;
  vmOutboundMarkBase = netCfg.vmOutboundMarkBase;
  vmOutboundRules = lib.filter (v: v != null) (
    map (
      vm:
      if (vm.outbound-via-node or null) != null then
        {
          iface = vm.iface;
          mark = vmOutboundMarkBase + vm.outbound-via-node;
        }
      else
        null
    ) vms
  );
  renderVmOutboundRules = lib.concatMapStrings (r: ''
    iifname "${r.iface}" ct direction original meta mark set ${toString r.mark}
    iifname "${r.iface}" ct direction original ct mark set 998
  '') vmOutboundRules;

  renderVmHairpinSnatRules =
    let
      vmVmMap = builtins.concatStringsSep ", " (
        map (vm: "${vmIpAddr vm} . ${vmIpAddr vm}") vms
      );
      hairpinDests =
        if pubIps != [ ] then
          "{ ${hostAddr}, $pubroutefix }"
        else
          "{ ${hostAddr} }";
    in
    ''
      ct original ip daddr ${hairpinDests} ip saddr . ip daddr { ${vmVmMap} } snat to 198.18.255.254
    '';

  renderAccessRule =
    {
      saddr ? null,
      iface ? null,
    }:
    access:
    let
      prefix = lib.concatStringsSep " " (
        lib.optional (iface != null) "iifname ${iface}" ++ lib.optional (saddr != null) "ip saddr ${saddr}"
      );
      line = suffix: "${lib.optionalString (prefix != "") "${prefix} "}${suffix} accept\n";
    in
    lib.optionalString (access.tcp != [ ]) (line "tcp dport ${portSet access.tcp}")
    + lib.optionalString (access.udp != [ ]) (line "udp dport ${portSet access.udp}")
    + lib.optionalString (access.ipProtocols != [ ]) (line "ip protocol ${portSet access.ipProtocols}");

  hostRangeCidr = addr.hostCidr;
  loopbackCidr = addr.bgpCidr;
  wgCidr = addr.wireguardCidr;
  internalSubnetsSet = "{ ${addr.containerCidr}, ${wgCidr}, ${loopbackCidr} }";

  renderVmSpecialHostAccess = lib.concatMapStrings (
    vm: renderAccessRule { saddr = vmSaddr vm; } vm.specialHostAccess
  ) vms;

  renderVmOutboundDropRules = lib.concatMapStrings (
    vm:
    lib.concatMapStrings (ip: ''
      ip saddr ${vmSaddr vm} ip daddr ${ip} drop
    '') vm.outboundDropIP
  ) vms;
  renderVmOutboundAllowOnlyRules = lib.concatMapStrings (
    vm:
    lib.optionalString (vm.outboundAllowOnlyIP != [ ]) (
      lib.concatMapStrings (ip: ''
        ip saddr ${vmSaddr vm} ip daddr ${ip} accept
      '') vm.outboundAllowOnlyIP
      + ''
        ip saddr ${vmSaddr vm} drop
      ''
    )
  ) vms;
in
{
  config = lib.mkIf netCfg.enabled {
    networking.firewall.enable = false;
    networking.nftables.enable = true;
    networking.nftables.ruleset = ''
            flush ruleset
            define wg_ifs = ${nftIfaceSet wgIfaceNames}
            define uplink_ifs = ${nftIfaceSet uplinkIfaceNames}
        ${lib.optionalString (pubIps != [ ]) ''
          define pubroutefix = { ${builtins.concatStringsSep ", " pubIps} }
        ''}
            table inet filter {
              chain INPUT {
                type filter hook input priority filter; policy drop;
                ct state invalid drop
                ct state { established, related } accept
                icmp type { echo-request, echo-reply } accept
                icmpv6 type { echo-request, nd-neighbor-solicit } accept
                iif "lo" accept
      ${renderAccessRule { } netCfg.openPorts}
      ${renderAccessRule { saddr = internalSubnetsSet; } netCfg.hostInternalPortAllNet}
      ${renderAccessRule { iface = "$wg_ifs"; saddr = wgCidr; } netCfg.specialWgAccess}
      ${renderAccessRule { iface = "$wg_ifs"; saddr = loopbackCidr; } netCfg.specialLoopbackAccess}
      ${renderAccessRule { saddr = hostRangeCidr; } netCfg.specialHostAccess}
      ${renderVmSpecialHostAccess}
              }
              chain FORWARD {
                type filter hook forward priority filter; policy accept;
                ct state invalid drop
      ${renderVmOutboundDropRules}
      ${renderVmOutboundAllowOnlyRules}
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
      ${renderVmSaddrRules}
              }
            }
            table ip nat {
              chain prerouting {
                type nat hook prerouting priority dstnat;
        ${renderUplinkDnatRules}
        ${lib.optionalString (pubIps != [ ]) (renderDestRules "$pubroutefix")}
        ${lib.optionalString (pubIps != [ ]) (renderDestRules "${hostAddr}")}
              }
              chain postrouting {
                type nat hook postrouting priority srcnat;
        ${renderVmHairpinSnatRules}
        ${renderUplinkMasqueradeRules}
              }
              chain output {
                type nat hook output priority dstnat; policy accept;
              }
            }
            table inet mangle {
              # populated at runtime by melinoe-route (nftAddPeer/nftRemovePeer)
              set melinoe_peer_marks {
                type mark
              }
              map melinoe_peer_ifaces {
                type ifname : mark
              }
              chain prerouting {
                type filter hook prerouting priority mangle;
      ${renderVmOutboundRules}
                iifname != $uplink_ifs ct direction reply ct mark 999 meta mark set ${toString netCfg.uplinkFwMark}
                iifname $uplink_ifs ct direction original ct mark != 999 ct mark set 999
                ct direction reply ct mark @melinoe_peer_marks meta mark set ct mark
                ct mark != 998 ct direction original ct mark != @melinoe_peer_marks ct mark set iifname map @melinoe_peer_ifaces
              }
              chain output {
                type route hook output priority mangle;
                ct direction reply ct mark 999 meta mark set ${toString netCfg.uplinkFwMark}
                ct direction reply ct mark @melinoe_peer_marks meta mark set ct mark
                ct mark 998 ip ttl set 62
              }
            }
    '';
  };
}
