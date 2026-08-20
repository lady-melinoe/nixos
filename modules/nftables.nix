{
  config,
  lib,
  melinoeNodeIntraIP,
  ...
}:
let
  cfg = config.melinoe;
  netCfg = config.melinoe.node.networking;
  addr = config.melinoe.cluster.networking;
  nodeID = cfg.node.id;
  hostAddr = melinoeNodeIntraIP nodeID;
  pubIps = lib.filter (ip: ip != null) (map (entry: entry.pub_ip or null) netCfg.uplinks);
  vms = cfg.cluster.virtualMachines;
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
  renderIfaceRules =
    ifaceExpr:
    lib.concatMapStrings (
      mapping:
      (renderProtoRule "tcp" mapping.tcp "iifname ${ifaceExpr}" mapping.dst)
      + (renderProtoRule "udp" mapping.udp "iifname ${ifaceExpr}" mapping.dst)
    ) natMappings;
  renderDestRules =
    destExpr:
    lib.concatMapStrings (
      mapping:
      (renderProtoRule "tcp" mapping.tcp "ip daddr ${destExpr}" mapping.dst)
      + (renderProtoRule "udp" mapping.udp "ip daddr ${destExpr}" mapping.dst)
    ) natMappings;
  nftIfaceSet =
    names: if names == [ ] then "{ }" else "{ ${lib.concatStringsSep ", " (map (n: "\"${n}\"") names)} }";
  vmIfaceNames = map (vm: vm.iface) vms;
  nodeIfaceNames = map (id: "node-${toString id}") (
    lib.filter (id: id != nodeID) (lib.range addr.nodeIdRange.min addr.nodeIdRange.max)
  );
  wgIfaceNames = map (peer: "wg-${toString peer.id}") netCfg.peers;
  vmOutboundMarkBase = netCfg.vmOutboundMarkBase;
  nodeOutboundMarkRange = "${toString vmOutboundMarkBase}-${toString (vmOutboundMarkBase + addr.nodeIdRange.max)}";
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
  '') vmOutboundRules;

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
            define vm_ifs = ${nftIfaceSet vmIfaceNames}
            define node_gre_ifs = ${nftIfaceSet nodeIfaceNames}
            define wg_ifs = ${nftIfaceSet wgIfaceNames}
            define gre_ctmark = { ${
              builtins.concatStringsSep ", " (
                builtins.genList (
                  i: "\"node-${toString (i)}\" : ${toString (vmOutboundMarkBase + i)}"
                ) (addr.nodeIdRange.max + 1)
              )
            } }
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
        ${renderIfaceRules "\"${netCfg.uplinkVeth}\""}
        ${lib.optionalString (pubIps != [ ]) (renderDestRules "$pubroutefix")}
        ${lib.optionalString (pubIps != [ ]) ''
          ip daddr $pubroutefix dnat to ${hostAddr}
        ''}
              }
              chain postrouting {
                type nat hook postrouting priority srcnat;
                oifname "${netCfg.uplinkVeth}" masquerade
              }
              chain output {
                type nat hook output priority dstnat; policy accept;
        ${lib.optionalString (pubIps != [ ]) (renderDestRules "$pubroutefix")}
        ${lib.optionalString (pubIps != [ ]) ''
          ip daddr $pubroutefix dnat to ${hostAddr}
        ''}
              }
            }
            table inet mangle {
              chain prerouting {
                type filter hook prerouting priority mangle;
      ${renderVmOutboundRules}
                iifname $vm_ifs ct direction reply ct mark ${nodeOutboundMarkRange} meta mark set ct mark
                iifname $node_gre_ifs ct direction original ct mark != ${nodeOutboundMarkRange} ct mark set iifname map $gre_ctmark
                iifname != ${netCfg.uplinkVeth} ct direction reply ct mark 999 meta mark set ${toString netCfg.uplinkFwMark}
                iifname ${netCfg.uplinkVeth} ct direction original ct mark != 999 ct mark set 999
              }
              chain output {
                type route hook output priority mangle;
                ct direction reply ct mark ${nodeOutboundMarkRange} meta mark set ct mark
                ct direction reply ct mark 999 meta mark set ${toString netCfg.uplinkFwMark}
              }
            }
    '';
  };
}
