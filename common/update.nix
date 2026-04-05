{ pkgs, ... }:
let
  update = pkgs.writeShellScriptBin "update" ''
    #!/usr/bin/env bash
    set -euo pipefail

    no_pull=0
    rebuild_mode="switch"
    hostname=""

    usage() {
      cat <<'EOF'
Usage: update [--no-pull] [--boot] [--hostname HOSTNAME]

Options:
  --no-pull         Skip git pull in /etc/nixos
  --boot            Run nixos-rebuild boot instead of switch
  --hostname NAME   Rebuild a different host than the local machine
  -h, --help        Show this help
EOF
    }

    while [ "$#" -gt 0 ]; do
      case "$1" in
        --no-pull)
          no_pull=1
          ;;
        --boot)
          rebuild_mode="boot"
          ;;
        --hostname)
          shift
          if [ "$#" -eq 0 ]; then
            echo "--hostname requires a value" >&2
            exit 1
          fi
          hostname="$1"
          ;;
        -h|--help)
          usage
          exit 0
          ;;
        *)
          echo "Unknown option: $1" >&2
          usage >&2
          exit 1
          ;;
      esac
      shift
    done

    if [ -z "$hostname" ]; then
      hostname="$(hostname -s)"
    fi

    if [ "$no_pull" -eq 0 ]; then
      git -C /etc/nixos pull
    fi

    exec nixos-rebuild "$rebuild_mode" --flake "/etc/nixos/#''${hostname}"
  '';
in
{
  environment.systemPackages = [ update ];
}
