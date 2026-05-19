{ config, lib, pkgs, ... }:

let
  cfg = config.melinoe;
  nodeID = cfg.nodeId;
  pubIps = lib.filter (ip: ip != null) (map (entry: entry.pub_ip or null) cfg.internet);

  routeScript = pkgs.writeTextFile {
    name = "melinoe-route.py";
    destination = "/bin/melinoe-route";
    executable = true;
    text = ''
#!/usr/bin/env python3

import json
import multiprocessing
import re
import subprocess
import sys
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer


BASE_PREFIX = "198.51.100."
INNER_PREFIX = "198.18.0."
TUN_PREFIX = "node-"

LOCAL_NODE_ID = ${toString nodeID}

HOST = f"{INNER_PREFIX}{LOCAL_NODE_ID}"
PORT = 60198

PUB_IPS = [
${lib.concatMapStringsSep "\n" (ip: "    \"${ip}\",") pubIps}
]

ADVERTISED_ROUTES = [
${lib.concatMapStringsSep "\n" (route: "    \"${route}\",") cfg.advertisedRoutes}
]


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


def normalize_prefix(route):
    if "/" in route:
        return route
    return f"{route}/32"


def valid_ipv4_or_prefix(value):
    return re.match(r"^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+(/[0-9]+)?$", value) is not None


def build_route_list():
    routes = []

    result = run(["ip", "route"]).stdout

    for line in result.splitlines():
        if "dev vm-" not in line:
            continue

        parts = line.split()
        if not parts:
            continue

        routes.append(normalize_prefix(parts[0]))

    for pub_ip in PUB_IPS:
        routes.append(normalize_prefix(pub_ip))

    for route in ADVERTISED_ROUTES:
        routes.append(normalize_prefix(route))

    return "\n".join(routes) + "\n"


def local_advertised_routes():
    return set(build_route_list().splitlines())


def ping_ms(target, *, mark=None):
    cmd = ["ping", "-n", "-c", "3", "-W", "1"]

    if mark is not None:
        cmd += ["-m", str(mark)]

    cmd.append(target)

    result = run(cmd, check=False, timeout=5)

    if result.returncode != 0:
        return None

    match = re.search(r"rtt min/avg/max/(?:mdev|stddev) = [0-9.]+/([0-9.]+)/", result.stdout)

    if not match:
        return None

    return float(match.group(1))


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
            response = build_route_list()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.end_headers()
            self.wfile.write(response.encode())
        except Exception as e:
            self.send_response(500)
            self.end_headers()
            self.wfile.write(f"Error: {e}\n".encode())


def serve_route_list():
    server = HTTPServer((HOST, PORT), RequestHandler)
    server.serve_forever()


def fetch_remote_route_list(remote_inner):
    try:
        with urllib.request.urlopen(f"http://{remote_inner}:60198/list", timeout=5) as response:
            body = response.read().decode()
    except Exception:
        body = ""

    routes = []

    for line in body.splitlines():
        route = line.strip()

        if not valid_ipv4_or_prefix(route):
            continue

        routes.append(normalize_prefix(route))

    return sorted(set(routes))


def get_current_routes_for_tunnel(tun):
    result = ip("route", "show", "dev", tun, check=False).stdout

    routes = []

    for line in result.splitlines():
        parts = line.split()

        if not parts:
            continue

        routes.append(normalize_prefix(parts[0]))

    return sorted(set(routes))


def deploy_once():
    local_node_id = str(LOCAL_NODE_ID)

    if not re.match(r"^[0-9]+$", local_node_id):
        print(f"invalid local node id: {local_node_id}", file=sys.stderr)
        return

    bgp_json_text = run(["vtysh", "-c", "show bgp ipv4 json"]).stdout

    if not bgp_json_text:
        print("failed to fetch BGP JSON", file=sys.stderr)
        return

    bgp = json.loads(bgp_json_text)

    local_vip = f"{BASE_PREFIX}{local_node_id}"
    local_inner = f"{INNER_PREFIX}{local_node_id}"

    local_inner_route = f"{local_inner}/32"
    local_vip_route = f"{local_vip}/32"

    locally_advertised = local_advertised_routes()

    if local_vip_route not in ip("a", "show", "dev", "lo").stdout:
        ip("a", "add", local_vip_route, "dev", "lo")

    peer_hops = {}

    for prefix, paths in bgp.get("routes", {}).items():
        match = re.match(r"^198\.51\.100\.([0-9]+)/32$", prefix)

        if not match:
            continue

        if not paths:
            continue

        path = paths[0].get("path", "")

        if path == "":
            continue

        remote_id = match.group(1)
        hops = len([part for part in path.strip().split(" ") if part])

        peer_hops[remote_id] = hops

    remote_node_ids = sorted(peer_hops.keys(), key=lambda value: int(value))

    existing_tunnels = []

    existing = ip("-o", "link", "show", "type", "ipip", check=False).stdout

    for line in existing.splitlines():
        parts = line.split(": ", 2)

        if len(parts) < 2:
            continue

        tun = parts[1].split("@", 1)[0].rstrip(":")

        if tun.startswith(TUN_PREFIX):
            existing_tunnels.append(tun)

    for tun in existing_tunnels:
        remote_id = tun.removeprefix(TUN_PREFIX)

        if not re.match(r"^[0-9]+$", remote_id):
            continue

        table = str(1000 + int(remote_id))

        if remote_id not in remote_node_ids:
            ip("route", "flush", "table", table, check=False)
            ip("rule", "del", "fwmark", table, "lookup", table, check=False)
            ip("link", "set", tun, "down", check=False)
            ip("tunnel", "del", tun, check=False)

    route_candidates = {}
    managed_tunnels = set()

    for remote_id in remote_node_ids:
        if not re.match(r"^[0-9]+$", remote_id):
            continue

        hops = peer_hops.get(remote_id)

        if not isinstance(hops, int) or hops < 1:
            continue

        remote_vip = f"{BASE_PREFIX}{remote_id}"
        remote_inner = f"{INNER_PREFIX}{remote_id}"
        tun = f"{TUN_PREFIX}{remote_id}"
        table = str(1000 + int(remote_id))
        table_mark = 1000 + int(remote_id)

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
                continue

            ip(
                "addr",
                "replace",
                f"{local_inner}/32",
                "peer",
                f"{remote_inner}/32",
                "dev",
                tun,
            )

        ip("link", "set", tun, "mtu", "1400", check=False)
        ip("link", "set", tun, "up", check=False)

        ip("rule", "del", "fwmark", table, "lookup", table, check=False)
        ip("rule", "add", "fwmark", table, "lookup", table, check=False)

        ip("route", "replace", "default", "dev", tun, "table", table)

        managed_tunnels.add(tun)

        host_ping = ping_ms(remote_inner, mark=table_mark)

        if host_ping is None:
            continue

        new_routes = fetch_remote_route_list(remote_inner)
        tunnel_peer_route = f"{remote_inner}/32"

        for prefix in new_routes:
            if prefix == local_inner_route:
                continue

            if prefix == tunnel_peer_route:
                continue

            if prefix in locally_advertised:
                continue

            route_candidates.setdefault(prefix, []).append({
                "tun": tun,
                "remote_id": remote_id,
                "ping": host_ping,
                "hops": hops,
            })

    wanted = {}

    for prefix, candidates in route_candidates.items():
        best = min(candidates, key=lambda candidate: (
            candidate["ping"],
            candidate["hops"],
            int(candidate["remote_id"]),
        ))

        wanted[prefix] = best["tun"]

        metric = max(1, int(round(best["ping"])))

        ip(
            "route",
            "replace",
            prefix,
            "dev",
            best["tun"],
            "metric",
            str(metric),
            check=False,
        )

    for tun in sorted(set(existing_tunnels) | managed_tunnels):
        current_routes = get_current_routes_for_tunnel(tun)

        for prefix in current_routes:
            if prefix == local_inner_route:
                continue

            if prefix.startswith(INNER_PREFIX):
                continue

            if wanted.get(prefix) != tun:
                ip("route", "del", prefix, "dev", tun, check=False)


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
    server_thread = threading.Thread(target=serve_route_list, daemon=True)
    server_thread.start()

    deploy_loop()


if __name__ == "__main__":
    main()
    '';
  };

in {
  imports = [
    ./networking/bgp.nix
    ./networking/nftables.nix
    ./networking/uplink.nix
    ./networking/wireguard.nix
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

  systemd.services.melinoe-route = {
    description = "Melinoe route daemon";
    wantedBy = [ "multi-user.target" ];
    after = [ "network-online.target" ];
    wants = [ "network-online.target" ];

    path = [
      pkgs.coreutils
      pkgs.iproute2
      pkgs.frr
      pkgs.iputils
    ];

    serviceConfig = {
      ExecStart = "${pkgs.python3}/bin/python ${routeScript}/bin/melinoe-route";
      Restart = "always";
      RestartSec = "5s";
      LogLevelMax = "warning";
    };
  };
}
