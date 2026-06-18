{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.melinoe;
  nodeID = cfg.nodeId;

  routeConfig = pkgs.writeText "melinoe-route.json" (
    builtins.toJSON {
      node_id = nodeID;
      hostname = config.networking.hostName;
      pub_ips = lib.filter (ip: ip != null) (map (entry: entry.pub_ip or null) cfg.internet);
      advertised_routes = cfg.advertisedRoutes;
      regions = cfg.regions;
    }
  );

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
      import argparse
      from concurrent.futures import ThreadPoolExecutor
      from http.server import BaseHTTPRequestHandler, HTTPServer

      BASE_PREFIX = "198.51.100."
      INNER_PREFIX = "198.18.0."
      TUN_PREFIX = "node-"
      HOST = None
      PORT = 60198

      def load_config(path):
          with open(path) as f:
              return json.load(f)

      def parse_args():
          p = argparse.ArgumentParser()
          p.add_argument("--config", required=True)
          return p.parse_args()

      LOCAL_NODE_ID = None
      PUB_IPS = []
      ADVERTISED_ROUTES = []

      # split local vs learned peer regions
      LOCAL_REGIONS = []
      PEER_REGIONS = {}

      HOSTNAME = None

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

      def format_region(node_id):
          node_id = str(node_id)

          if node_id == str(LOCAL_NODE_ID):
              return " ".join(LOCAL_REGIONS)

          return " ".join(PEER_REGIONS.get(node_id, []))

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

          body = "\n".join(sorted(routes)) + "\n"

          header_1 = "// melinoe-list 0.0.1"
          header_2 = f"// {LOCAL_NODE_ID}: {HOSTNAME} - {format_region(LOCAL_NODE_ID)}"

          return header_1 + "\n" + header_2 + "\n" + body

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

          # learn peer region metadata (backward compatible)
          parse_remote_header(body)

          routes = set()
          for line in body.splitlines():
              line = line.split("//", 1)[0].strip()
              if not line:
                  continue
              add_route(routes, line.strip())

          return sorted(routes)

      def parse_remote_header(body):
          """
          Backward compatible parser for:
          // <id>: <hostname> - AAA BBB CCC
          """
          for line in body.splitlines():
              if not line.startswith("//"):
                  continue

              line = line[2:].strip()

              m = re.match(r"^(\d+):\s*.*?\-\s*(.*)$", line)
              if not m:
                  continue

              node_id = m.group(1)
              regions_str = m.group(2).strip()

              if regions_str:
                  PEER_REGIONS[node_id] = regions_str.split()
              else:
                  PEER_REGIONS[node_id] = []

      def get_current_routes_for_tunnel(tun):
          result = ip("-j", "route", "show", "dev", tun, "proto", "198", check=False)
          if result.returncode != 0:
              return []
          routes = set()
          for route in json.loads(result.stdout):
              dst = route.get("dst")
              if dst is not None:
                  add_route(routes, dst)
          return sorted(routes)

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

          ip("addr", "replace", f"{local_inner}/32", "peer", f"{remote_inner}/32", "dev", tun, check=False)
          ip("link", "set", tun, "mtu", "1400", check=False)
          ip("link", "set", tun, "up", check=False)
          ip("rule", "del", "fwmark", table, "lookup", table, check=False)
          ip("rule", "add", "fwmark", table, "lookup", table, check=False)
          ip("route", "replace", "default", "dev", tun, "table", table)
          return True

      def region_priority(remote_id):
          local_regions = LOCAL_REGIONS
          remote_regions = PEER_REGIONS.get(str(remote_id), [])

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
                  ip("route", "replace", prefix, "dev", tun, "metric", "1", "proto", "198", check=False)

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
          global LOCAL_NODE_ID, PUB_IPS, ADVERTISED_ROUTES, LOCAL_REGIONS, HOST, HOSTNAME

          args = parse_args()
          cfg = load_config(args.config)

          LOCAL_NODE_ID = cfg["node_id"]
          PUB_IPS = cfg["pub_ips"]
          ADVERTISED_ROUTES = cfg["advertised_routes"]
          LOCAL_REGIONS = cfg["regions"]
          HOSTNAME = cfg["hostname"]

          HOST = f"{INNER_PREFIX}{LOCAL_NODE_ID}"

          threading.Thread(target=serve_route_list, daemon=True).start()
          deploy_loop()

      if __name__ == "__main__":
          main()
    '';
  };
in
{
  systemd.services.melinoe-route = {
    description = "Melinoe route daemon";
    wantedBy = [ "multi-user.target" ];
    after = [ "network-online.target" ];
    wants = [ "network-online.target" ];
    path = [
      pkgs.coreutils
      pkgs.iproute2
    ];
    serviceConfig = {
      ExecStart = "${pkgs.python3}/bin/python ${routeScript}/bin/melinoe-route --config ${routeConfig}";
      Restart = "always";
      RestartSec = "5s";
      LogLevelMax = "warning";
    };
  };
}
