{ config, lib, pkgs, ... }:

let
  cfg = config.melinoe;
  nodeID = cfg.nodeId;

  pubIps =
    lib.filter (ip: ip != null)
      (map (entry: entry.pub_ip or null) cfg.internet);

  pyList = values:
    lib.concatMapStringsSep "\n" (value: "    \"${value}\",") values;

  # BGP / WireGuard shared node settings.
  bgpAs = 64512 + nodeID;
  basePort = 64512;
  wgPrefix = "198.19.3";
  peers = map (p: { id = p.id; addr = "${wgPrefix}.${toString p.id}"; }) cfg.peers;
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

  natMappings = [
    { dst = "198.18.1.5"; tcp = [ 8080 ]; udp = [ 8080 ]; }
    { dst = "198.18.1.16"; tcp = [ 80 443 1080 1443 ]; udp = [ 80 443 1080 1443 ]; }
    { dst = "198.18.1.6"; tcp = [ 993 25 465 ]; udp = [ ]; }
    { dst = "198.18.1.8"; tcp = [ 25565 ]; udp = [ 25565 ]; }
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

  # Uplink / inet namespace config generation.
  ns = "ip netns exec inet";

  uplinkIface = idx: uplink:
    if builtins.length uplink.iface > 1
    then "bond${toString idx}"
    else builtins.head uplink.iface;

  uplinkIpAddr = uplink:
    builtins.head (lib.splitString "/" uplink.ip);

  inetNftRuleset = pkgs.writeText "melinoe-inet.nft" ''
    table ip nat {
      chain prerouting {
        type nat hook prerouting priority dstnat; policy accept;
        ${lib.concatStringsSep "\n" (lib.imap0 (idx: uplink:
          ''iifname "${uplinkIface idx uplink}" ip daddr ${uplinkIpAddr uplink} dnat to ${hostAddr}''
        ) cfg.internet)}
      }

      chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        ${lib.concatStringsSep "\n" (lib.imap0 (idx: uplink:
          ''oifname "${uplinkIface idx uplink}" masquerade''
        ) cfg.internet)}
      }
    }
  '';

  mkUplinkScript = idx: uplink:
    let
      ifaces = uplink.iface;
      isBonded = builtins.length ifaces > 1;
      iface = uplinkIface idx uplink;

      ipParts = lib.splitString "/" uplink.ip;
      ipAddr = builtins.head ipParts;
      ipPrefixFromIp =
        if builtins.length ipParts > 1
        then builtins.elemAt ipParts 1
        else null;

      subnetParts =
        if uplink.subnet != null
        then lib.splitString "/" uplink.subnet
        else null;

      subnetPrefix =
        if subnetParts != null && builtins.length subnetParts > 1
        then builtins.elemAt subnetParts 1
        else null;

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

  peerToCfg = peer:
    let
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

  # Melinoe route daemon.
  routeScript = pkgs.writeTextFile {
    name = "melinoe-route.py";
    destination = "/bin/melinoe-route";
    executable = true;
    text = ''
#!/usr/bin/env python3

import ipaddress
import json
import multiprocessing
import re
import subprocess
import sys
import threading
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, HTTPServer


BASE_PREFIX = "198.51.100."
INNER_PREFIX = "198.18.0."
TUN_PREFIX = "node-"

LOCAL_NODE_ID = ${toString nodeID}

HOST = f"{INNER_PREFIX}{LOCAL_NODE_ID}"
PORT = 60198

PUB_IPS = [
${pyList pubIps}
]

ADVERTISED_ROUTES = [
${pyList cfg.advertisedRoutes}
]

REGIONS = ${builtins.toJSON cfg.regions}


def run(cmd, *, check=True, input=None, timeout=300):
    return subprocess.run(
        cmd,
        check=check,
        input=input,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=timeout,
    )


def ip(*args, check=True):
    return run(["ip", *args], check=check)


def node_id_valid(value):
    return re.match(r"^[0-9]+$", value) is not None


def normalize_prefix(value):
    try:
        return str(ipaddress.ip_network(value, strict=False))
    except ValueError:
        return None


def add_route(routes, value):
    route = normalize_prefix(value)

    if route is not None:
        routes.add(route)


def build_route_list():
    routes = set()

    result = ip("-j", "route", check=False)

    if result.returncode == 0:
        for route in json.loads(result.stdout):
            dev = route.get("dev")
            dst = route.get("dst")

            if dev is None or dst is None:
                continue

            if dev.startswith("vm-"):
                add_route(routes, dst)

    for pub_ip in PUB_IPS:
        add_route(routes, pub_ip)

    for route in ADVERTISED_ROUTES:
        add_route(routes, route)

    return "\n".join(sorted(routes)) + "\n"


def local_advertised_routes():
    return set(build_route_list().splitlines())


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
            body = build_route_list()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.end_headers()
            self.wfile.write(body.encode())
        except Exception as e:
            self.send_response(500)
            self.end_headers()
            self.wfile.write(f"Error: {e}\n".encode())


def serve_route_list():
    HTTPServer((HOST, PORT), RequestHandler).serve_forever()


def fetch_remote_route_list(remote_inner):
    try:
        with urllib.request.urlopen(f"http://{remote_inner}:60198/list", timeout=5) as response:
            body = response.read().decode()
    except Exception:
        return []

    routes = set()

    for line in body.splitlines():
        add_route(routes, line.strip())

    return sorted(routes)


def get_current_routes_for_tunnel(tun):
    result = ip("-j", "route", "show", "dev", tun, check=False)

    if result.returncode != 0:
        return []

    routes = set()

    for route in json.loads(result.stdout):
        dst = route.get("dst")

        if dst is not None:
            add_route(routes, dst)

    return sorted(routes)


def region_priority(remote_id):
    local_regions = REGIONS.get(str(LOCAL_NODE_ID), [])
    remote_regions = REGIONS.get(str(remote_id), [])

    def same(index):
        return (
            len(local_regions) > index
            and len(remote_regions) > index
            and local_regions[index] == remote_regions[index]
        )

    return (
        not same(2),
        not same(1),
        not same(0),
        int(remote_id),
    )


def get_bgp_route_table_peer_ids():
    result = ip("route", "show", "proto", "bgp", check=False)
    remote_ids = set()

    for line in result.stdout.splitlines():
        parts = line.split()

        if not parts:
            continue

        match = re.match(r"^198\.51\.100\.([0-9]+)(?:/32)?$", parts[0])

        if match is None:
            continue

        remote_id = match.group(1)

        if remote_id != str(LOCAL_NODE_ID):
            remote_ids.add(remote_id)

    return sorted(remote_ids, key=region_priority)


def existing_melinoe_tunnels():
    result = ip("-o", "link", "show", "type", "ipip", check=False)
    tunnels = []

    for line in result.stdout.splitlines():
        parts = line.split(": ", 2)

        if len(parts) < 2:
            continue

        tun = parts[1].split("@", 1)[0].rstrip(":")

        if tun.startswith(TUN_PREFIX):
            tunnels.append(tun)

    return tunnels


def flush_tunnel(tun):
    remote_id = tun.removeprefix(TUN_PREFIX)

    if not node_id_valid(remote_id):
        return

    table = str(1000 + int(remote_id))

    ip("route", "flush", "table", table, check=False)
    ip("rule", "del", "fwmark", table, "lookup", table, check=False)
    ip("link", "set", tun, "down", check=False)
    ip("tunnel", "del", tun, check=False)


def ensure_tunnel(remote_id, local_vip, local_inner, remote_vip, remote_inner, tun, table):
    if ip("link", "show", tun, check=False).returncode != 0:
        created = ip(
            "tunnel",
            "add",
            tun,
            "mode",
            "ipip",
            "local",
            local_vip,
            "remote",
            remote_vip,
            "ttl",
            "64",
            check=False,
        )

        if created.returncode != 0:
            print(f"failed to create {tun}: {created.stderr}", file=sys.stderr)
            return False

    ip(
        "addr",
        "replace",
        f"{local_inner}/32",
        "peer",
        f"{remote_inner}/32",
        "dev",
        tun,
        check=False,
    )

    ip("link", "set", tun, "mtu", "1400", check=False)
    ip("link", "set", tun, "up", check=False)

    ip("rule", "del", "fwmark", table, "lookup", table, check=False)
    ip("rule", "add", "fwmark", table, "lookup", table, check=False)

    ip("route", "replace", "default", "dev", tun, "table", table)

    return True


def compute_desired_routes(peer_routes, local_inner_route, locally_advertised):
    desired = {}

    for state, routes in peer_routes:
        for prefix in routes:
            if prefix == local_inner_route:
                continue

            if prefix == state["tunnel_peer_route"]:
                continue

            if prefix in locally_advertised:
                continue

            if prefix not in desired:
                desired[prefix] = state["tun"]

    return desired


def reconcile_all_routes(peer_state, desired_routes):
    current_routes = {}

    tunnel_peer_routes = {
        state["tunnel_peer_route"]
        for state in peer_state
    }

    for state in peer_state:
        tun = state["tun"]

        for prefix in get_current_routes_for_tunnel(tun):
            if prefix in tunnel_peer_routes:
                continue

            current_routes[prefix] = tun

    for prefix, tun in desired_routes.items():
        if current_routes.get(prefix) != tun:
            ip("route", "replace", prefix, "dev", tun, "metric", "1", check=False)

    for prefix, tun in current_routes.items():
        if desired_routes.get(prefix) != tun:
            ip("route", "del", prefix, "dev", tun, check=False)


def deploy_once():
    local_node_id = str(LOCAL_NODE_ID)

    if not node_id_valid(local_node_id):
        print(f"invalid local node id: {local_node_id}", file=sys.stderr)
        return

    local_vip = f"{BASE_PREFIX}{local_node_id}"
    local_inner = f"{INNER_PREFIX}{local_node_id}"

    local_inner_route = f"{local_inner}/32"
    local_vip_route = f"{local_vip}/32"

    ip("addr", "replace", local_vip_route, "dev", "lo", check=False)

    locally_advertised = local_advertised_routes()
    remote_node_ids = get_bgp_route_table_peer_ids()

    for tun in existing_melinoe_tunnels():
        remote_id = tun.removeprefix(TUN_PREFIX)

        if remote_id not in remote_node_ids:
            flush_tunnel(tun)

    peer_state = []

    for remote_id in remote_node_ids:
        if not node_id_valid(remote_id):
            continue

        remote_vip = f"{BASE_PREFIX}{remote_id}"
        remote_inner = f"{INNER_PREFIX}{remote_id}"
        tun = f"{TUN_PREFIX}{remote_id}"
        table = str(1000 + int(remote_id))

        if not ensure_tunnel(remote_id, local_vip, local_inner, remote_vip, remote_inner, tun, table):
            continue

        peer_state.append({
            "remote_inner": remote_inner,
            "tun": tun,
            "tunnel_peer_route": f"{remote_inner}/32",
        })

    if not peer_state:
        return

    with ThreadPoolExecutor(max_workers=min(32, len(peer_state))) as executor:
        fetched_routes = list(executor.map(
            lambda state: fetch_remote_route_list(state["remote_inner"]),
            peer_state,
        ))

    desired_routes = compute_desired_routes(
        list(zip(peer_state, fetched_routes)),
        local_inner_route,
        locally_advertised,
    )

    reconcile_all_routes(peer_state, desired_routes)


def deploy_worker():
    try:
        deploy_once()
    except Exception as e:
        print(f"deploy failed: {e}", file=sys.stderr)


def deploy_loop():
    while True:
        process = multiprocessing.Process(target=deploy_worker)
        process.start()
        process.join(300)

        if process.is_alive():
            print("deploy timed out", file=sys.stderr)
            process.terminate()
            process.join(5)

            if process.is_alive():
                process.kill()
                process.join()

        time.sleep(3)


def main():
    threading.Thread(target=serve_route_list, daemon=True).start()
    deploy_loop()


if __name__ == "__main__":
    main()
    '';
  };

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

  networking.firewall.enable = false;
  networking.nftables.enable = true;
  networking.nftables.ruleset = ''
    flush ruleset
    define vm_ifs = "vm-*"
    define node_gre_ifs = "node-*"
    define wg_ifs = "wg-*"
    define gre_ctmark = { ${builtins.concatStringsSep ", " (builtins.genList (i: "\"node-${toString (i)}\" : ${toString (1000 + i)}") 255)} }
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
        iifname $wg_ifs ip saddr 198.51.100.0/24 ip protocol 4 accept
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
      in ''

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

        ${let
          firstUplink = lib.head cfg.internet;
        in lib.optionalString (firstUplink.gateway != null) ''
          ${ns} ip route replace default via ${firstUplink.gateway} dev ${uplinkIface 0 firstUplink}
        ''}
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

  systemd.services.melinoe-route = {
    description = "Melinoe route daemon";
    wantedBy = [ "multi-user.target" ];
    after = [ "network-online.target" ];
    wants = [ "network-online.target" ];

    path = [
      pkgs.coreutils
      pkgs.iproute2
      pkgs.frr
    ];

    serviceConfig = {
      ExecStart = "${pkgs.python3}/bin/python ${routeScript}/bin/melinoe-route";
      Restart = "always";
      RestartSec = "5s";
      LogLevelMax = "warning";
    };
  };
}
