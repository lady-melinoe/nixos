{ config, lib, pkgs, ... }:
let
  cfg = config.melinoe;
  nodeID = cfg.nodeId;
  pubIps = lib.filter (ip: ip != null) (map (entry: entry.pub_ip or null) cfg.internet);
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

    def add_route(self, routes, route):
        if "/" not in route:
            route = f"{route}/32"
        routes.append(route)

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
                self.add_route(routes, ip)
            for pub_ip in [
${lib.concatMapStringsSep "\n" (ip: "    \"${ip}\",") pubIps}
            ]:
                self.add_route(routes, pub_ip)
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

}
