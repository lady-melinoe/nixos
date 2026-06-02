{
  pkgs,
  lib,
  nixosConfigurations,
}:

let
  stripCidr = ip: lib.head (lib.splitString "/" ip);

  hostMeta = lib.mapAttrs (
    name: cfg:
    let
      c = cfg.config;
      nodeId = toString c.melinoe.nodeId;

      internetIps = lib.concatMap (
        uplink: [ uplink.ip ] ++ lib.optional (uplink.pub_ip != null) uplink.pub_ip
      ) c.melinoe.internet;
    in
    {
      hostname = c.networking.hostName;
      sshTarget = "${name}.infra.melinoe.xyz";

      principals = lib.unique (
        [
          c.networking.hostName
          "${c.networking.hostName}.intra.melinoe.xyz"
          "${c.networking.hostName}.infra.melinoe.xyz"
        ]
        ++ map stripCidr internetIps
        ++ [
          "198.19.3.${nodeId}"
          "198.18.0.${nodeId}"
        ]
      );
    }
  ) nixosConfigurations;

  hostMetaJson = builtins.toJSON hostMeta;

in
{
  inherit hostMeta;

  mel-ssh-host-ca = pkgs.writeShellApplication {
    name = "mel-ssh-host-ca";

    runtimeInputs = [
      pkgs.coreutils
      pkgs.jq
      pkgs.openssh
    ];

    checkPhase = "";

    text = ''
      set -euo pipefail

      hosts_json='${hostMetaJson}'
      ca_key="''${MEL_HOST_CA_KEY:-$HOME/ssh-ca-keys/ssh-host-ca}"
      validity="''${MEL_HOST_CERT_VALIDITY:-+30d}"
      workdir="''${MEL_HOST_CA_WORKDIR:-.ssh-host-ca-work}"
      host_key_path="''${MEL_HOST_KEY_PATH:-/etc/ssh/ssh_host_ed25519_key}"
      host_pubkey_path="''${MEL_HOST_PUBKEY_PATH:-/etc/ssh/ssh_host_ed25519_key.pub}"
      host_cert_path="''${MEL_HOST_CERT_PATH:-/etc/ssh/ssh_host_ed25519_key-cert.pub}"

      usage() {
        cat <<'EOF'
      Usage:
        mel-ssh-host-ca list
        mel-ssh-host-ca principals HOST
        mel-ssh-host-ca bootstrap HOST
        mel-ssh-host-ca retrieve HOST
        mel-ssh-host-ca sign HOST [PUBKEY]
        mel-ssh-host-ca deploy HOST [CERT]
        mel-ssh-host-ca rotate [--bootstrap] HOST
        mel-ssh-host-ca rotate-all [--bootstrap]
      EOF
      }

      names() {
        printf '%s\n' "$hosts_json" | jq -r 'keys[]' | sort
      }

      require_host() {
        local host="$1"
        if ! printf '%s\n' "$hosts_json" | jq -e --arg host "$host" 'has($host)' >/dev/null; then
          echo "Unknown host: $host" >&2
          names >&2
          exit 1
        fi
      }

      ssh_target() {
        printf '%s\n' "$hosts_json" | jq -r --arg host "$1" '.[$host].sshTarget'
      }

      principals_csv() {
        printf '%s\n' "$hosts_json" | jq -r --arg host "$1" '.[$host].principals | join(",")'
      }

      host_workdir() {
        printf '%s/%s\n' "$workdir" "$1"
      }

      ask_yes_no() {
        local prompt="$1"
        local answer

        [ -t 0 ] || return 1

        while true; do
          printf '%s [y/N] ' "$prompt" >&2
          read -r answer
          case "$answer" in
            y|Y|yes|YES) return 0 ;;
            n|N|no|NO|"") return 1 ;;
            *) echo "Please answer y or n." >&2 ;;
          esac
        done
      }

      bootstrap() {
        local host="$1"
        local target

        require_host "$host"
        target="$(ssh_target "$host")"

        ssh "root@$target" "
          set -euo pipefail
          if [ ! -s '$host_key_path' ]; then
            rm -f '$host_key_path' '$host_pubkey_path'
            ssh-keygen -t ed25519 -N \"\" -f '$host_key_path'
          fi
          if [ ! -s '$host_pubkey_path' ]; then
            ssh-keygen -y -f '$host_key_path' > '$host_pubkey_path'
          fi
          chown root:root '$host_key_path' '$host_pubkey_path'
          chmod 0600 '$host_key_path'
          chmod 0644 '$host_pubkey_path'
        "
      }

      retrieve() {
        local host="$1"
        local target
        local dir
        local out

        require_host "$host"
        target="$(ssh_target "$host")"
        dir="$(host_workdir "$host")"
        out="$dir/ssh_host_ed25519_key.pub"

        mkdir -p "$dir"

        if ! ssh "root@$target" "test -s '$host_pubkey_path' && cat '$host_pubkey_path'" > "$out"; then
          rm -f "$out"
          echo "Could not retrieve key" >&2
          return 42
        fi

        printf '%s\n' "$out"
      }

      sign_key() {
        local host="$1"
        local pubkey="''${2:-$(host_workdir "$host")/ssh_host_ed25519_key.pub}"
        local principals

        require_host "$host"

        [ -f "$ca_key" ] || { echo "Missing CA key" >&2; exit 1; }
        [ -f "$pubkey" ] || { echo "Missing pubkey" >&2; exit 1; }

        principals="$(principals_csv "$host")"

        rm -f "''${pubkey%.pub}-cert.pub"

        ssh-keygen \
          -s "$ca_key" \
          -I "$host" \
          -h \
          -n "$principals" \
          -V "$validity" \
          "$pubkey" >&2

        printf '%s\n' "''${pubkey%.pub}-cert.pub"
      }

      deploy_cert() {
        local host="$1"
        local cert="''${2:-$(host_workdir "$host")/ssh_host_ed25519_key-cert.pub}"
        local target

        require_host "$host"

        [ -f "$cert" ] || { echo "Missing cert" >&2; exit 1; }

        target="$(ssh_target "$host")"

        ssh "root@$target" "
          set -euo pipefail
          cat > '$host_cert_path'
          systemctl restart sshd
        " < "$cert"
      }

      rotate_one() {
        local host="$1"
        local mode="$2"
        local pubkey
        local cert

        if ! pubkey="$(retrieve "$host")"; then
          case "$mode" in
            always) bootstrap "$host"; pubkey="$(retrieve "$host")" ;;
            *) echo "missing key"; return 1 ;;
          esac
        fi

        cert="$(sign_key "$host" "$pubkey")"
        deploy_cert "$host" "$cert"
      }

      cmd="''${1:-}"
      shift || true

      case "$cmd" in
        list) names ;;
        principals) require_host "$1"; jq -r --arg host "$1" '.[$host].principals[]' <<< "$hosts_json" ;;
        bootstrap) bootstrap "$1" ;;
        retrieve) retrieve "$1" ;;
        sign) sign_key "$1" "''${2:-}" ;;
        deploy) deploy_cert "$1" "''${2:-}" ;;
        rotate) rotate_one "$1" "ask" ;;
        rotate-all)
          for h in $(names); do rotate_one "$h" "ask"; done
          ;;
        *) usage ;;
      esac
    '';
  };
}
