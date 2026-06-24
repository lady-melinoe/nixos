{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.melinoe;
  nodeID = cfg.nodeId;

  pubIps = lib.filter (ip: ip != null) (map (entry: entry.pub_ip or null) cfg.internet);

  pyList = values: lib.concatMapStringsSep "\n" (value: "    \"${value}\",") values;

  # BGP / WireGuard shared node settings.
  bgpAs = 64512 + nodeID;
  basePort = 64512;
  wgPrefix = "198.19.3";
  peers = map (p: {
    id = p.id;
    addr = "${wgPrefix}.${toString p.id}";
  }) cfg.peers;
  localWgAddr = "${wgPrefix}.${toString nodeID}";

  # BGP config generation.
  mkNeighborStanza = peer: ''
    neighbor ${peer.addr} remote-as ${toString (64512 + peer.id)}
    neighbor ${peer.addr} update-source ${localWgAddr}
    neighbor ${peer.addr} timers 1 3
  '';

  mkNeighborAfi = peer: ''
    neighbor ${peer.addr} activate
    neighbor ${peer.addr} route-map NODE-IN in
    neighbor ${peer.addr} route-map NODE-OUT out
  '';

  # nftables config generation.
  hostAddr = "198.18.0.${toString nodeID}";

  # Single source of truth for VM interfaces.
  # ip: bare address for single hosts, CIDR for ranges (e.g. "198.18.3.0/24").
  # tcp/udp: ports to NAT-forward to this VM (omit or leave empty for none).
  vms = [
    { iface = "vm-npm";          ip = "198.18.1.1"; }
    { iface = "vm-npmalt";       ip = "198.18.1.2"; }
    { iface = "vm-vaultwarden";  ip = "198.18.1.3"; }
    { iface = "vm-authentik";    ip = "198.18.1.4"; }
    { iface = "vm-homepage";     ip = "198.18.1.5";  tcp = [ 8080 ]; udp = [ 8080 ]; }
    { iface = "vm-mailserver";   ip = "198.18.1.6";  tcp = [ 993 25 465 ]; }
    { iface = "vm-radicale";     ip = "198.18.1.7"; }
    { iface = "vm-1710pack";     ip = "198.18.1.8";  tcp = [ 25565 ]; udp = [ 25565 ]; }
    { iface = "vm-website";      ip = "198.18.1.9"; }
    { iface = "vm-mmmanager";    ip = "198.18.1.10"; }
    { iface = "vm-drasl";        ip = "198.18.1.11"; tcp = [ 57843 ]; udp = [ 57843 ]; }
    { iface = "vm-calibre";      ip = "198.18.1.12"; }
    { iface = "vm-gitlab";       ip = "198.18.1.13"; }
    { iface = "vm-snappymail";   ip = "198.18.1.14"; }
    { iface = "vm-devicebridge"; ip = "198.18.3.0/24"; udp = [ 51820 ]; }
  ];

  # Helpers for working with the vms list.
  vmIpAddr = vm: builtins.head (lib.splitString "/" vm.ip);
  vmSaddr  = vm: if lib.hasInfix "/" vm.ip then vm.ip else "${vm.ip}/32";
  vmTcp    = vm: vm.tcp or [ ];
  vmUdp    = vm: vm.udp or [ ];

  # Derived natMappings — only VMs that have ports defined.
  natMappings = lib.filter (m: m.tcp != [ ] || m.udp != [ ]) (
    map (vm: { dst = vmIpAddr vm; tcp = vmTcp vm; udp = vmUdp vm; }) vms
  );

  # Derived raw table saddr spoof-prevention rules.
  renderVmSaddrRules = lib.concatMapStrings (vm: ''
          iifname "${vm.iface}" ip saddr != ${vmSaddr vm} drop
  '') vms;

  portSet =
    ports:
    if ports == [ ] then
      ""
    else if builtins.length ports == 1 then
      toString (builtins.head ports)
    else
      "{ ${builtins.concatStringsSep ", " (map toString ports)} }";

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

  # Uplink / inet namespace config generation.
  ns = "ip netns exec inet";

  uplinkIface =
    idx: uplink:
    if builtins.length uplink.iface > 1 then "bond${toString idx}" else builtins.head uplink.iface;

  uplinkIpAddr = uplink: builtins.head (lib.splitString "/" uplink.ip);

  inetNftRuleset = pkgs.writeText "melinoe-inet.nft" ''
    table ip nat {
      chain prerouting {
        type nat hook prerouting priority dstnat; policy accept;
        ${lib.concatStringsSep "\n" (
          lib.imap0 (
            idx: uplink:
            ''iifname "${uplinkIface idx uplink}" ip daddr ${uplinkIpAddr uplink} dnat to ${hostAddr}''
          ) cfg.internet
        )}
      }

      chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        ${lib.concatStringsSep "\n" (
          lib.imap0 (idx: uplink: ''oifname "${uplinkIface idx uplink}" masquerade'') cfg.internet
        )}
      }
    }
  '';

  mkUplinkScript =
    idx: uplink:
    let
      ifaces = uplink.iface;
      isBonded = builtins.length ifaces > 1;
      iface = uplinkIface idx uplink;

      ipParts = lib.splitString "/" uplink.ip;
      ipAddr = builtins.head ipParts;
      ipPrefixFromIp = if builtins.length ipParts > 1 then builtins.elemAt ipParts 1 else null;

      subnetParts = if uplink.subnet != null then lib.splitString "/" uplink.subnet else null;

      subnetPrefix =
        if subnetParts != null && builtins.length subnetParts > 1 then
          builtins.elemAt subnetParts 1
        else
          null;

      ipWithPrefix =
        if subnetPrefix != null then
          "${ipAddr}/${subnetPrefix}"
        else if ipPrefixFromIp != null then
          uplink.ip
        else
          "${ipAddr}/32";
    in
    ''
      ${lib.concatMapStringsSep "\n" (name: ''
        ip link set ${name} down
        ip link set ${name} netns inet
      '') ifaces}

      ${lib.optionalString isBonded ''
        ${ns} ip link add ${iface} type bond mode 802.3ad
        ${lib.optionalString (uplink.lacpRate != null) ''
          ${ns} ip link set ${iface} type bond lacp_rate ${uplink.lacpRate}
        ''}
        ${lib.concatMapStringsSep "\n" (name: ''
          ${ns} ip link set ${name} master ${iface}
        '') ifaces}
      ''}

      ${ns} ip link set ${iface} up
      ${ns} ip addr flush dev ${iface}
      ${ns} ip addr replace ${ipWithPrefix} dev ${iface}

      ${lib.optionalString (uplink.subnet != null) ''
        ${ns} ip route replace ${uplink.subnet} dev ${iface}
      ''}
    '';

  # WireGuard config generation.
  keys = lib.filterAttrs (_: v: v != null) (lib.mapAttrs (_: node: node.wgPubkey) cfg.publicNodes);

  peerToCfg =
    peer:
    let
      pid = peer.id;
      allowed = lib.unique (peer.allowedIPs ++ [ "${wgPrefix}.${toString pid}/32" ]);
    in
    {
      publicKey = keys."${toString pid}";
      allowedIPs = allowed;
      endpoint = "${peer.endpoint}:${toString (basePort + cfg.nodeId)}";
      persistentKeepalive = peer.persistentKeepalive;
    };

  runtimeShell = "${pkgs.runtimeShell}";
  ipBin = "${pkgs.iproute2}/bin/ip";

  mkInterface =
    peer:
    let
      ifName = "wg-${toString peer.id}";
    in
    {
      name = ifName;
      value = {
        address = [ ];
        listenPort = basePort + peer.id;
        privateKeyFile = "/etc/melinoe/wg.privatekey";
        extraOptions = {
          FwMark = 51820;
        };
        peers = [ (peerToCfg peer) ];
        postUp = ''
          ${runtimeShell} -c '${ipBin} route del ${wgPrefix}.${toString peer.id}/32 dev ${ifName} || true'
          ${runtimeShell} -c '${ipBin} address replace ${localWgAddr}/32 peer ${wgPrefix}.${toString peer.id}/32 dev ${ifName}'
          ${runtimeShell} -c '${ipBin} route del 198.19.3.0/24 dev ${ifName} || true'
          ${runtimeShell} -c '${ipBin} route del 198.51.100.0/24 dev ${ifName} || true'
        '';
      };
    };

  interfaces = lib.listToAttrs (map mkInterface cfg.peers);
  ports = map (peer: basePort + peer.id) cfg.peers;

  wgWatchdogScript = pkgs.writeShellScript "melinoe-wg-watchdog" ''
    #!/usr/bin/env bash
    set -euo pipefail

    for IFACE in ${lib.concatStringsSep " " (map (peer: "wg-${toString peer.id}") cfg.peers)}; do
      UNIT="wg-quick-''${IFACE}.service"

      if systemctl is-active --quiet "$UNIT" && ip link show "$IFACE" >/dev/null 2>&1; then
        continue
      fi

      systemctl restart "$UNIT"
    done
  '';

in
{
  imports = [
    ./melinoe-route.nix
  ];

  networking.domain = "infra.melinoe.xyz";
  networking.useDHCP = false;

  networking.interfaces.lo.ipv4.addresses = [
    {
      address = "198.51.100.${toString nodeID}";
      prefixLength = 32;
    }
    {
      address = "198.18.0.${toString nodeID}";
      prefixLength = 32;
    }
  ];

  networking.firewall.enable = false;
  networking.nftables.enable = true;
  networking.nftables.ruleset = ''
        flush ruleset
        define vm_ifs = "vm-*"
        define node_gre_ifs = "node-*"
        define wg_ifs = "wg-*"
        define gre_ctmark = { ${
          builtins.concatStringsSep ", " (
            builtins.genList (i: "\"node-${toString (i)}\" : ${toString (1000 + i)}") 255
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
            tcp dport 22 accept  # ssh
            tcp dport {80, 443, 1080, 1443} accept  # haproxy
            udp dport {80, 443, 1080, 1443} accept  # haproxy
            tcp dport 2049 accept  # nfs
            udp dport 2049 accept  # nfs
            tcp dport 8008 accept  # incus
            ip saddr 198.18.0.0/15 tcp dport 5201 accept                  # iperf3
            iifname $wg_ifs ip saddr 198.19.3.0/24 tcp dport 179 accept   # bgp, internal only, over wireguard subnet
            iifname $wg_ifs ip saddr 198.51.100.0/24 ip protocol 4 accept # ipip, internal only, over bgp routed subnet
            ip saddr 198.18.0.0/24 tcp dport 60198 accept                 #  melinoe-route protocol
            ip saddr 198.18.1.5 tcp dport 61208 accept                    # glances for monitoring
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
  ${renderVmSaddrRules}

            iifname $vm_ifs ip saddr 198.18.0.0/24 drop
            iifname $vm_ifs ip saddr != 198.18.0.0/16 drop
          }
        }

        table ip nat {
          chain prerouting {
            type nat hook prerouting priority dstnat;
    ${renderIfaceRules "\"inet0\""}
    ${lib.optionalString (pubIps != [ ]) (renderDestRules "$pubroutefix")}
    ${lib.optionalString (pubIps != [ ]) ''
      ip daddr $pubroutefix dnat to ${hostAddr}
    ''}
          }
          chain postrouting {
            type nat hook postrouting priority srcnat;
            oifname "inet0" masquerade
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
            iifname $vm_ifs ct direction reply ct mark 1000-1254 meta mark set ct mark
            iifname $node_gre_ifs ct direction original ct mark != 1000-1254 ct mark set iifname map $gre_ctmark

            iifname != inet0 ct direction reply ct mark 999 meta mark set 51820
            iifname inet0 ct direction original ct mark != 999 ct mark set 999
          }
          chain output {
            type route hook output priority mangle;
            ct direction reply ct mark 1000-1254 meta mark set ct mark
            ct direction reply ct mark 999 meta mark set 51820
          }
        }
  '';

  services.frr = {
    bgpd.enable = true;
    config =
      let
        neighborLines = lib.concatMapStrings mkNeighborStanza peers;
        neighborAfiLines = lib.concatMapStrings mkNeighborAfi peers;
      in
      ''

                ip prefix-list NODE-LOOPS permit 198.51.100.0/24 le 32

                route-map NODE-IN permit 10
                 match ip address prefix-list NODE-LOOPS

                route-map NODE-OUT permit 10
                 match ip address prefix-list NODE-LOOPS

                router bgp ${toString bgpAs}
                  bgp router-id 198.51.100.${toString nodeID}
                  bgp fast-convergence
        ${neighborLines}

                  address-family ipv4 unicast
                    network 198.51.100.${toString nodeID}/32
        ${neighborAfiLines}
                  exit-address-family
                !
      '';
  };

  networking.wg-quick.interfaces = lib.mkIf (cfg.peers != [ ]) interfaces;
  melinoe.wgPorts = lib.mkIf (cfg.peers != [ ]) ports;

  systemd.services.melinoe-inet-setup = lib.mkIf (cfg.internet != [ ]) {
    description = "Configure inet netns veth pair for host<->inet connectivity";
    after = [ "network-pre.target" ];
    wants = [ "network-pre.target" ];
    wantedBy = [ "multi-user.target" ];

    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;

      ExecStart = pkgs.writeShellScript "melinoe-inet-setup" ''
        ip netns add inet 2>/dev/null || true
        ${ns} ip link set lo up

        ip link del inet0 2>/dev/null || true
        ip link add inet0 type veth peer name main
        ip link set main netns inet

        ip addr replace ${hostAddr}/32 dev inet0
        ip link set inet0 up

        ${ns} ip link set main up
        ${ns} ip addr replace 198.18.0.255/32 dev main
        ${ns} ip route replace ${hostAddr}/32 dev main

        ip route replace 198.18.0.255 dev inet0
        ip route replace default via 198.18.0.255 dev inet0

        ip rule del fwmark 51820 lookup 51820 >/dev/null 2>&1 || true
        ip rule add fwmark 51820 lookup 51820
        ip route replace default via 198.18.0.255 dev inet0 table 51820

        ${ns} sysctl -w net.ipv4.conf.default.rp_filter=0
        ${ns} sysctl -w net.ipv4.conf.all.rp_filter=0

        ${lib.concatStringsSep "\n" (lib.imap0 mkUplinkScript cfg.internet)}

        ${ns} nft delete table ip nat 2>/dev/null || true
        ${ns} nft -f ${inetNftRuleset}

        ${
          let
            firstUplink = lib.head cfg.internet;
          in
          lib.optionalString (firstUplink.gateway != null) ''
            ${ns} ip route replace default via ${firstUplink.gateway} dev ${uplinkIface 0 firstUplink}
          ''
        }
      '';
    };

    path = [
      pkgs.iproute2
      pkgs.nftables
      pkgs.procps
    ];
  };

  systemd.services.melinoe-wg-watchdog = lib.mkIf (cfg.peers != [ ]) {
    description = "Restart WireGuard interfaces that are down";
    after = [ "network-online.target" ];
    wants = [ "network-online.target" ];
    path = [
      pkgs.iproute2
      pkgs.systemd
      pkgs.gnugrep
    ];
    serviceConfig = {
      Type = "oneshot";
      ExecStart = wgWatchdogScript;
    };
  };

  systemd.timers.melinoe-wg-watchdog = lib.mkIf (cfg.peers != [ ]) {
    wantedBy = [ "timers.target" ];
    timerConfig = {
      OnBootSec = "2min";
      OnUnitActiveSec = "2min";
      AccuracySec = "30s";
      Persistent = true;
    };
  };
}
