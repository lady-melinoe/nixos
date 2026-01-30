{ config, lib, pkgs, ... }:
let
  inherit (lib) mkOption types;
  nodeID = config.melinoe.nodeId;
  incusStorageSource = config.melinoe.incusDefaultStorageSource;
  incusRootSize = config.melinoe.incusRootSize;
  incusPreseedEnabled = config.melinoe.enableIncusPreseed;
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
          def do_GET(self):
              if self.path != "/list":
                  self.send_response(404)
                  self.end_headers()
                  self.wfile.write(b"Not Found\n")
                  return
              try:
                  result = subprocess.check_output(["ip", "route"], text=True)
                  routes = []
                  for line in result.splitlines():
                      if "dev vm-" not in line:
                          continue
                      ip = line.split()[0]
                      if "/" not in ip:
                          ip = f"{ip}/32"
                      routes.append(ip)
                  response = "\\n".join(routes) + "\\n"
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
      description = "Melinoe resident route list server";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      serviceConfig = {
        ExecStart = "${pkgs.python3}/bin/python ${pkgs.writeTextFile {
          name = \"melinoe-route-list.py\";
          destination = \"/bin/melinoe-route-list\";
          executable = true;
          text = ''
            #!/usr/bin/env python3
            import subprocess
            from http.server import BaseHTTPRequestHandler, HTTPServer

            HOST = "198.18.0.${toString nodeID}"
            PORT = 60198

            class RequestHandler(BaseHTTPRequestHandler):
                def do_GET(self):
                    if self.path != "/list":
                        self.send_response(404)
                        self.end_headers()
                        self.wfile.write(b"Not Found\n")
                        return
                    try:
                        result = subprocess.check_output(["ip", "route"], text=True)
                        routes = []
                        for line in result.splitlines():
                            if "dev vm-" not in line:
                                continue
                            ip = line.split()[0]
                            if "/" not in ip:
                                ip = f"{ip}/32"
                            routes.append(ip)
                        response = "\\n".join(routes) + "\\n"
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
        }}/bin/melinoe-route-list";
        Restart = "on-failure";
        RestartSec = "5s";
      };
    };
  };
}
