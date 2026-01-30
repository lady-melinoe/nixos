{ config, lib, pkgs, ... }:
let
  inherit (lib) mkOption types;
  nodeID = config.melinoe.nodeId;
  incusStorageSource = config.melinoe.incusDefaultStorageSource;
  incusRootSize = config.melinoe.incusRootSize;
  incusPreseedEnabled = config.melinoe.enableIncusPreseed;
  routeListServer = pkgs.writeTextFile {
    name = "melinoe-route-list.py";
    destination = "/bin/melinoe-route-list";
    executable = true;
    text = ''
      #!/usr/bin/env python3
      import http.server
      import os
      import subprocess
      import sys


      def gather_routes():
        res = subprocess.run(["ip", "-o", "route", "show"], capture_output=True, text=True)
        routes = []
        for line in res.stdout.splitlines():
          line = line.strip()
          if not line:
            continue
          if " dev vm-" not in line:
            continue
          # first field is prefix (may be bare IP)
          prefix = line.split()[0]
          if "/" not in prefix:
            prefix = f"{prefix}/32"
          routes.append(prefix)
        return routes


      class Handler(http.server.SimpleHTTPRequestHandler):
        def __init__(self, *args, directory=None, **kwargs):
          super().__init__(*args, directory=directory, **kwargs)

        def do_GET(self):
          path = self.path.split("?", 1)[0]
          if path == "/list":
            routes = gather_routes()
            body = "\n".join(routes) + ("\n" if routes else "")
            data = body.encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
          return super().do_GET()

        def log_message(self, format, *args):
          return


      def main():
        if len(sys.argv) != 4:
          print("Usage: melinoe-route-list <bind_host> <port> <docroot>")
          sys.exit(1)
        host = sys.argv[1]
        port = int(sys.argv[2])
        docroot = sys.argv[3]
        os.chdir(docroot)
        handler = lambda *args, **kwargs: Handler(*args, directory=docroot, **kwargs)
        with http.server.ThreadingHTTPServer((host, port), handler) as httpd:
          httpd.serve_forever()


      if __name__ == "__main__":
        main()
    '';
  };
  configureIface = pkgs.writeShellScriptBin "configure-iface" ''
    #!/usr/bin/env bash
    IFACE="$1"
    ACTION="$2"
    IP="$3"
    RES_FILE="/etc/melinoe/residents/list"

    if [ -z "$IFACE" ] || [ -z "$ACTION" ] || [ -z "$IP" ]; then
      echo "Usage: $0 <iface> <up|down> <ip>"
      exit 1
    fi

    mkdir -p "$(dirname "$RES_FILE")"
    touch "$RES_FILE"

    case "$ACTION" in
      up)
        echo 1 > /proc/sys/net/ipv4/conf/$IFACE/forwarding
        echo 1 > /proc/sys/net/ipv4/conf/$IFACE/proxy_arp
        ip addr replace 198.18.0.${toString nodeID}/32 dev "$IFACE"

        if ! grep -Fxq "$IP" "$RES_FILE"; then
          echo "$IP" >> "$RES_FILE"
        fi
        ;;
      down)
        sed -i "\|^$IP\$|d" "$RES_FILE"
        ;;
      *)
        echo "Invalid action: $ACTION"
        echo "Use: up or down"
        exit 1
        ;;
    esac
  '';
in {
  options.melinoe.incusRootSize = mkOption {
    type = types.str;
    default = "35GiB";
    description = "Size for the default Incus root disk profile.";
  };
  options.melinoe.enableIncusPreseed = mkOption {
    type = types.bool;
    default = false;
    description = "Whether to apply the default Incus preseed (storage pool and profile).";
  };

  config = {
    virtualisation.incus.enable = true;
    users.users.melinoe.extraGroups = [ "incus-admin" ];
    virtualisation.incus.preseed = lib.mkIf incusPreseedEnabled {
      networks = [];
      profiles = [
        {
          devices = {
            root = {
              path = "/";
              pool = "default";
              size = incusRootSize;
              type = "disk";
            };
          };
          name = "default";
        }
      ];
      storage_pools = [
        {
          config = {
            source = incusStorageSource;
          };
          driver = "btrfs";
          name = "default";
        }
      ];
    };

    system.activationScripts.incusConfigureIface = {
      text = ''
        mkdir -p /etc/incus/hooks
        ln -sf ${configureIface}/bin/configure-iface /etc/incus/hooks/configure-iface
      '';
    };

    services.glances.enable = true;
    services.glances.port = 61208;
    services.glances.extraArgs = [ "--webserver" "-B" "198.18.0.${toString nodeID}" ];

    systemd.services.melinoe-route-list = {
      description = "Melinoe resident list server";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      serviceConfig = {
        ExecStartPre = "${pkgs.coreutils}/bin/mkdir -p /etc/melinoe/residents";
        ExecStart = "${routeListServer}/bin/melinoe-route-list 198.18.0.${toString nodeID} 60198 /etc/melinoe/residents";
        WorkingDirectory = "/etc/melinoe/residents";
        Restart = "on-failure";
        RestartSec = "5s";
        Environment = "PATH=${pkgs.iproute2}/bin:${pkgs.coreutils}/bin:${pkgs.gnugrep}/bin:${pkgs.gawk}/bin";
      };
    };
  };
}
