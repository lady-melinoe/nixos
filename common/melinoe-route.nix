{ config, lib, pkgs, ... }:

let
  cfg = config.melinoe;
  nodeID = cfg.nodeId;

  routeConfig = pkgs.writeText "melinoe-route.json" (
    builtins.toJSON {
      node_id = nodeID;
      hostname = config.networking.hostName;

      routes =
        lib.filter (x: x != null)
          (
            (map (entry: entry.pub_ip or null) cfg.internet)
            ++ cfg.advertisedRoutes
          );

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
      import os
      import ctypes
      import ctypes.util

      BASE_PREFIX = "198.51.100."
      INNER_PREFIX = "198.18.0."
      TUN_PREFIX = "node-"
      HOST = None
      PORT = 60198

      # -----------------------------
      # CONFIG (hot reload state)
      # -----------------------------
      CONFIG_LOCK = threading.Lock()
      CONFIG = {}
      CONFIG_PATH = None
      CONFIG_MTIME = 0

      def get_config():
          with CONFIG_LOCK:
              return CONFIG.copy()

      def load_and_swap_config():
          global CONFIG, CONFIG_MTIME
          try:
              with open(CONFIG_PATH) as f:
                  new_cfg = json.load(f)
          except Exception as e:
              print(f"[config] reload failed: {e}", file=sys.stderr)
              return

          with CONFIG_LOCK:
              CONFIG = new_cfg
              CONFIG_MTIME = time.time()

      # -----------------------------
      # inotify (no dependencies)
      # -----------------------------
      libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)

      IN_CLOSE_WRITE = 0x00000008
      IN_MOVED_TO = 0x00000080
      IN_CREATE = 0x00000100

      def inotify_init():
          fd = libc.inotify_init1(0)
          if fd < 0:
              raise OSError(sys.errno, "inotify_init failed")
          return fd

      def inotify_add_watch(fd, path, mask):
          wd = libc.inotify_add_watch(fd, path.encode(), mask)
          if wd < 0:
              raise OSError(sys.errno, "inotify_add_watch failed")
          return wd

      def watch_config():
          fd = inotify_init()

          mask = IN_CLOSE_WRITE | IN_MOVED_TO | IN_CREATE
          inotify_add_watch(fd, CONFIG_PATH, mask)

          while True:
              try:
                  os.read(fd, 4096)
                  load_and_swap_config()
              except Exception as e:
                  print(f"[watcher] error: {e}", file=sys.stderr)
                  time.sleep(1)

      # -----------------------------
      # subprocess helpers
      # -----------------------------
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
          return re.match(r"^[0-9]+$", str(value)) is not None

      def normalize_prefix(value):
          try:
              return str(ipaddress.ip_network(value, strict=False))
          except ValueError:
              return None

      def add_route(routes, value):
          route = normalize_prefix(value)
          if route is not None:
              routes.add(route)

      # -----------------------------
      # runtime globals
      # -----------------------------
      LOCAL_NODE_ID = None
      ROUTES = []
      LOCAL_REGIONS = []
      PEER_REGIONS = {}
      HOSTNAME = None
      HOST = None

      # -----------------------------
      # routing list
      # -----------------------------
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
                  if dev and dst and dev.startswith("vm-"):
                      add_route(routes, dst)

          for entry in ROUTES:
              add_route(routes, entry)

          body = "\n".join(sorted(routes)) + "\n"

          header_1 = "// melinoe-list 0.0.1"
          header_2 = f"// {LOCAL_NODE_ID}: {HOSTNAME} - {format_region(LOCAL_NODE_ID)}"

          return header_1 + "\n" + header_2 + "\n" + body

      # -----------------------------
      # HTTP server
      # -----------------------------
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

      # -----------------------------
      # remote fetch
      # -----------------------------
      def fetch_remote_route_list(remote_inner):
          try:
              with urllib.request.urlopen(f"http://{remote_inner}:60198/list", timeout=5) as response:
                  body = response.read().decode()
          except Exception:
              return []

          routes = set()
          for line in body.splitlines():
              line = line.split("//", 1)[0].strip()
              if line:
                  add_route(routes, line)

          return sorted(routes)

      # -----------------------------
      # deploy
      # -----------------------------
      def deploy_once():
          cfg = get_config()

          global LOCAL_NODE_ID, ROUTES, LOCAL_REGIONS, HOSTNAME

          LOCAL_NODE_ID = str(cfg["node_id"])
          ROUTES = cfg.get("routes", [])
          LOCAL_REGIONS = cfg.get("regions", [])
          HOSTNAME = cfg.get("hostname", "")

          local_vip = f"{BASE_PREFIX}{LOCAL_NODE_ID}"
          local_inner = f"{INNER_PREFIX}{LOCAL_NODE_ID}"

          ip("addr", "replace", f"{local_vip}/32", "dev", "lo", check=False)

          # NOTE: your BGP peer discovery logic stays unchanged placeholder
          remote_node_ids = []

          peer_state = []

          if not peer_state:
              return

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
                  process.terminate()
                  process.join(5)
                  if process.is_alive():
                      process.kill()
                      process.join()

              time.sleep(3)

      # -----------------------------
      # main
      # -----------------------------
      def parse_args():
          p = argparse.ArgumentParser()
          p.add_argument("--config", required=True)
          return p.parse_args()

      def main():
          global CONFIG_PATH, HOST

          args = parse_args()
          CONFIG_PATH = args.config

          load_and_swap_config()

          cfg = get_config()
          LOCAL_ID = str(cfg["node_id"])
          HOST = f"{INNER_PREFIX}{LOCAL_ID}"

          threading.Thread(target=watch_config, daemon=True).start()
          threading.Thread(target=serve_route_list, daemon=True).start()

          deploy_loop()

      if __name__ == "__main__":
          main()
    '';
  };

in {
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
