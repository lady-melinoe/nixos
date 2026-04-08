{ config, lib, pkgs, ... }:
let
  cfg = config.melinoe;
  nodeID = cfg.nodeId;
  keys = lib.filterAttrs (_: v: v != null) (lib.mapAttrs (_: node: node.wgPubkey) cfg.publicNodes);
  basePort = 64512;
  wgPrefix = "198.19.3";
  peers = map (p: { id = p.id; addr = "${wgPrefix}.${toString p.id}"; }) cfg.peers;
  bgpAs = 64512 + nodeID;
  localWgAddr = "${wgPrefix}.${toString nodeID}";
  firstUplink = lib.head cfg.internet;
  hostAddr = "198.18.0.${toString cfg.nodeId}";
  pubRouteFix = map (entry: entry.ip) cfg.internet;
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
  peerToCfg = peer: let
    pid = peer.id;
    allowed = lib.unique (peer.allowedIPs ++ [ "${wgPrefix}.${toString pid}/32" ]);
  in {
    publicKey = keys."${toString pid}";
    allowedIPs = allowed;
    endpoint = "${peer.endpoint}:${toString (basePort + cfg.nodeId)}";
    persistentKeepalive = peer.persistentKeepalive;
  };
  runtimeShell = "${pkgs.runtimeShell}";
  ipBin = "${pkgs.iproute2}/bin/ip";
  mkInterface = peer:
    let
      ifName = "wg-${toString peer.id}";
    in {
      name = ifName;
      value = {
        address = [ ];
        listenPort = basePort + peer.id;
        privateKeyFile = "/etc/melinoe/wg.privatekey";
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
  mkUplinkScript = idx: uplink:
    let
      ifaces = uplink.iface;
      isBonded = builtins.length ifaces > 1;
      iface = if isBonded then "bond${toString idx}" else builtins.head ifaces;
      ipParts = lib.splitString "/" uplink.ip;
      ipAddr = builtins.head ipParts;
      ipPrefixFromIp = if builtins.length ipParts > 1 then builtins.elemAt ipParts 1 else null;

      subnetParts = if uplink.subnet != null then lib.splitString "/" uplink.subnet else null;
      subnetPrefix = if subnetParts != null && builtins.length subnetParts > 1 then builtins.elemAt subnetParts 1 else null;

      ipWithPrefix =
        if subnetPrefix != null then "${ipAddr}/${subnetPrefix}"
        else if ipPrefixFromIp != null then uplink.ip
        else "${ipAddr}/32";
    in
    ''
      ${lib.concatMapStringsSep "\n" (name: ''
        ip link set ${name} down
        ip link set ${name} netns inet
      '') ifaces}
      ${lib.optionalString (!isBonded) ''
        ip netns exec inet ip link set ${iface} up
      ''}
      ${lib.optionalString isBonded ''
        ip netns exec inet ip link add ${iface} type bond mode 802.3ad
        ${lib.optionalString (uplink.lacpRate != null) ''
          ip netns exec inet ip link set ${iface} type bond lacp_rate ${uplink.lacpRate}
        ''}
        ip netns exec inet ip link set ${iface} up
        ${lib.concatMapStringsSep "\n" (name: ''
          ip netns exec inet ip link set ${name} master ${iface}
        '') ifaces}
      ''}
      ip netns exec inet ip addr flush dev ${iface}
      ip netns exec inet ip addr replace ${ipWithPrefix} dev ${iface}
      ${lib.optionalString (uplink.subnet != null) ''
        ip netns exec inet ip route replace ${uplink.subnet} dev ${iface}
      ''}
      ip netns exec inet iptables -t nat -C POSTROUTING -o ${iface} -j MASQUERADE
      ip netns exec inet iptables -t nat -A POSTROUTING -o ${iface} -j MASQUERADE
      ip netns exec inet iptables -t nat -C PREROUTING -d ${ipAddr} -j DNAT --to-destination ${hostAddr}
      ip netns exec inet iptables -t nat -C PREROUTING -i ${iface} -d ${ipAddr} -j DNAT --to-destination ${hostAddr}
      ip netns exec inet iptables -t nat -A PREROUTING -i ${iface} -d ${ipAddr} -j DNAT --to-destination ${hostAddr}
    '';
  routeListScript = pkgs.writeTextFile {
    name = "melinoe-route-list.py";
    destination = "/bin/melinoe-route-list";
    executable = true;
    text = ''
      #!/usr/bin/env python3
      import subprocess
      from http.server import BaseHTTPRequestHandler, HTTPServer

      HOST = "198.18.0.${toString nodeID}"
      PORT = 60198

      class RequestHandler(BaseHTTPRequestHandler):
          def log_message(self, format, *args):
              return

          def do_GET(self):
              if self.path != "/list":
                  self.send_response(404)
                  self.end_headers()
                  self.wfile.write(b"Not Found\n")
                  return
              try:
                  result = subprocess.check_output(["/run/current-system/sw/bin/ip", "route"], text=True)
                  routes = []
                  for line in result.splitlines():
                      if "dev vm-" not in line: continue
                      ip = line.split()[0]
                      if "/" not in ip: ip = f"{ip}/32"
                      routes.append(ip)
                  response = "\n".join(routes) + "\n"
                  self.send_response(200)
                  self.send_header("Content-Type", "text/plain")
                  self.end_headers()
                  self.wfile.write(response.encode())
              except Exception as e:
                  self.send_response(500)
                  self.end_headers()
                  self.wfile.write(f"Error: {e}\\n".encode())

      if __name__ == "__main__":
          server = HTTPServer((HOST, PORT), RequestHandler)
          server.serve_forever()
    '';
  };
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
  routeDeployScript = pkgs.writeShellScript "route-deploy.sh" ''
    #!/usr/bin/env bash

    BASE_PREFIX="198.51.100."
    INNER_PREFIX="198.18.0."
    TUN_PREFIX="node-"

    for cmd in vtysh jq ip curl grep awk sed sort uniq; do
      command -v "$cmd" >/dev/null 2>&1 || { echo "missing command: $cmd" >&2; exit 1; }
    done

    normalize_prefix() {
        case "$1" in
            */*) echo "$1" ;;
            *)   echo "$1/32" ;;
        esac
    }

    LOCAL_NODE_ID=${toString nodeID}

    if ! echo "$LOCAL_NODE_ID" | grep -Eq '^[0-9]+$'; then
      echo "invalid local node id: $LOCAL_NODE_ID" >&2
      exit 1
    fi

    BGP_JSON=$(vtysh -c 'show bgp ipv4 json')

    [ -n "$BGP_JSON" ] || { echo "failed to fetch BGP JSON" >&2; exit 1; }

    LOCAL_VIP="$BASE_PREFIX$LOCAL_NODE_ID"
    LOCAL_INNER="$INNER_PREFIX$LOCAL_NODE_ID"
    LOCAL_INNER_ROUTE="$LOCAL_INNER/32"

    REMOTE_NODE_IDS=$(
      jq -r '
        .routes
        | to_entries[]
        | select(.key | test("^198\\.51\\.100\\.[0-9]+/32$"))
        | .value[0] as $p
        | select($p.path != "")
        | .key
      ' <<<"$BGP_JSON" |
      awk -F"[./]" '{print $4}' |
      sort -n | uniq
    )

    EXISTING_TUNNELS=$(
      ip -o link show type gre 2>/dev/null |
      awk -F': ' '{print $2}' |
      cut -d@ -f1 |
      sed 's/:$//' |
      grep "^$TUN_PREFIX" || true
    )

    # Tear down GRE state for peers that disappeared from BGP
    for TUN in $EXISTING_TUNNELS; do
      RID=$(printf '%s\n' "$TUN" | sed "s/^$TUN_PREFIX//")
      if ! echo "$RID" | grep -Eq '^[0-9]+$'; then
        continue
      fi
      TABLE=$((1000 + RID))
      if ! grep -qx "$RID" <<<"$REMOTE_NODE_IDS"; then
        ip route flush table "$TABLE" >/dev/null 2>&1 || true
        ip rule del fwmark "$TABLE" lookup "$TABLE" >/dev/null 2>&1 || true
        ip link set "$TUN" down >/dev/null 2>&1 || true
        ip tunnel del "$TUN" >/dev/null 2>&1 || true
      fi
    done

    # Ensure tunnels and policy routing exist, then sync prefixes per peer
    for RID in $REMOTE_NODE_IDS; do
      if ! echo "$RID" | grep -Eq '^[0-9]+$'; then
        continue
      fi
      R_VIP="$BASE_PREFIX$RID"
      R_INNER="$INNER_PREFIX$RID"
      TUN="$TUN_PREFIX$RID"
      TABLE=$((1000 + RID))

      if ! ip link show "$TUN" >/dev/null 2>&1; then
      ip tunnel add "$TUN" mode gre \
          local "$LOCAL_VIP" \
          remote "$R_VIP" \
          ttl 64 || continue
        ip addr replace "$LOCAL_INNER/32" peer "$R_INNER/32" dev "$TUN"
      fi

      ip link set "$TUN" up || true
      ip rule del fwmark "$TABLE" lookup "$TABLE" >/dev/null 2>&1 || true
      ip rule add fwmark "$TABLE" lookup "$TABLE" >/dev/null 2>&1 || true
      ip route replace default dev "$TUN" table "$TABLE"

      ROUTE_LIST=$(curl -m 5 -sf "http://$R_INNER:60198/list" || echo "")

      NEW_ROUTES=$(
        echo "$ROUTE_LIST" |
        grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+(/[0-9]+)?$' |
        while read -r p; do normalize_prefix "$p"; done |
        sort -u
      )

      # Currently installed routes for this tunnel
      CURRENT_ROUTES=$(
        ip route show dev "$TUN" |
        awk '{print $1}' |
        while read -r p; do normalize_prefix "$p"; done |
        sort -u
      )

      GRE_PEER_ROUTE="$R_INNER/32"

      for PREFIX in $NEW_ROUTES; do
        case "$PREFIX" in
          "$LOCAL_INNER_ROUTE") continue ;;
          "$GRE_PEER_ROUTE")    continue ;;
        esac

        if ! grep -qx "$PREFIX" <<<"$CURRENT_ROUTES"; then
          ip route replace "$PREFIX" dev "$TUN"
        fi
      done

      for PREFIX in $CURRENT_ROUTES; do
        case "$PREFIX" in
          "$GRE_PEER_ROUTE") continue ;;
          "$LOCAL_INNER_ROUTE") continue ;;
        esac

        if ! grep -qx "$PREFIX" <<<"$NEW_ROUTES"; then
          ip route del "$PREFIX" dev "$TUN"
        fi
      done
    done
  '';
  mkNeighborStanza = peer: ''
    neighbor ${peer.addr} remote-as ${toString (64512 + peer.id)}
    neighbor ${peer.addr} update-source ${localWgAddr}
  '';
  mkNeighborAfi = peer: ''
    neighbor ${peer.addr} activate
    neighbor ${peer.addr} route-map NODE-IN in
    neighbor ${peer.addr} route-map NODE-OUT out
  '';
  mkBfdStanza = peer: ''
    peer ${peer.addr} interface wg-${toString peer.id}
      transmit-interval 1000
      receive-interval 1000
      detect-multiplier 3
      no shutdown
    !
  '';
in {
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
  networking.wg-quick.interfaces = lib.mkIf (cfg.peers != [ ]) interfaces;
  melinoe.wgPorts = lib.mkIf (cfg.peers != [ ]) ports;
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
        ip saddr 198.18.0.0/15 tcp dport 5201 accept # iperf3
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
      }
      chain postrouting {
        type nat hook postrouting priority srcnat;
        oifname "inet0" masquerade
      }
      chain output {
        type nat hook output priority dstnat; policy accept;
${lib.optionalString (pubRouteFix != [ ]) (renderDestRules "$pubroutefix")}
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
  systemd.services.melinoe-inet-setup = lib.mkIf (cfg.internet != [ ]) {
    description = "Configure inet netns veth pair for host<->inet connectivity";
    after = [ "network-pre.target" ];
    wants = [ "network-pre.target" ];
    wantedBy = [ "multi-user.target" ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
      ExecStart = pkgs.writeShellScript "melinoe-inet-setup" ''
        ip netns add inet
        ip netns exec inet ip link set lo up
        ip link del inet0
        ip link add inet0 type veth peer name main
        ip link set main netns inet
        ip addr replace ${hostAddr}/32 dev inet0
        ip link set inet0 up
        ip netns exec inet ip link set main up
        ip netns exec inet ip addr replace 198.18.0.255/32 dev main
        ip netns exec inet ip route replace ${hostAddr}/32 dev main
        ip route add 198.18.0.255 dev inet0
        ip route add default via 198.18.0.255 dev inet0
        ip netns exec inet sysctl -w net.ipv4.conf.default.rp_filter=0
        ip netns exec inet sysctl -w net.ipv4.conf.all.rp_filter=0

        ${lib.concatStringsSep "\n" (lib.imap0 mkUplinkScript cfg.internet)}

        # this one is only for the first interface for when we add multi iface support
        ${lib.optionalString (firstUplink.gateway != null) ''
          ip netns exec inet ip route add default via ${firstUplink.gateway} dev ${if builtins.length firstUplink.iface > 1 then "bond0" else builtins.head firstUplink.iface}
        ''}
      '';
    };
    path = [ pkgs.iproute2 pkgs.iptables pkgs.procps ];
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
      ExecStart = "${pkgs.coreutils}/bin/timeout 30 ${routeDeployScript}";
      LogLevelMax = "warning";
    };
  };

  systemd.services.melinoe-route-list = {
    description = "Melinoe resident route list server";
    wantedBy = [ "multi-user.target" ];
    after = [ "network-online.target" ];
    wants = [ "network-online.target" ];
    serviceConfig = {
      Environment = "PATH=/run/current-system/sw/bin";
      ExecStart = "${pkgs.python3}/bin/python ${routeListScript}/bin/melinoe-route-list";
      Restart = "on-failure";
      RestartSec = "5s";
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

  systemd.services.melinoe-wg-watchdog = lib.mkIf (cfg.peers != [ ]) {
    description = "Restart WireGuard interfaces that are down";
    after = [ "network-online.target" ];
    wants = [ "network-online.target" ];
    path = [ pkgs.iproute2 pkgs.systemd pkgs.gnugrep ];
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

  services.frr = {
    bgpd.enable = true;
    bfdd.enable = true;
    config =
      let
        neighborLines = lib.concatMapStrings mkNeighborStanza peers;
        neighborAfiLines = lib.concatMapStrings mkNeighborAfi peers;
        bfdLines = lib.concatMapStrings mkBfdStanza peers;
      in ''

        ip prefix-list NODE-LOOPS permit 198.51.100.0/24 le 32

        route-map NODE-IN permit 10
         match ip address prefix-list NODE-LOOPS

        route-map NODE-OUT permit 10
         match ip address prefix-list NODE-LOOPS

        router bgp ${toString bgpAs}
          bgp router-id 198.51.100.${toString nodeID}
${neighborLines}

          address-family ipv4 unicast
            network 198.51.100.${toString nodeID}/32
${neighborAfiLines}
          exit-address-family
        !
        bfd
${bfdLines}
        !
      '';
  };
}
