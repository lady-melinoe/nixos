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
      nodeId = toString c.melinoe.node.id;

      internetIps = lib.concatMap (
        uplink:
        [ uplink.ip ]
        ++ lib.optional (uplink.pub_ip != null) uplink.pub_ip
      ) c.melinoe.node.networking.uplinks;
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
pkgs.writeShellApplication {
  name = "mel-ssh-provision";

  runtimeInputs = with pkgs; [
    coreutils
    jq
    openssh
  ];

  text = ''
    set -euo pipefail

    hosts_json='${hostMetaJson}'
    ca_key="''${MEL_HOST_CA_KEY:-$HOME/ssh-ca-keys/ssh-host-ca}"
    validity="''${MEL_HOST_CERT_VALIDITY:-+30d}"
    host_cert_path="''${MEL_HOST_CERT_PATH:-/etc/ssh/ssh_host_ed25519_key-cert.pub}"

    USER_SSH_AUTH_SOCK="''${SSH_AUTH_SOCK:-}"
    CLEANUP_DIRS=()
    CA_AGENT_PID=""

    cleanup() {
      [ -n "$CA_AGENT_PID" ] &&
        kill "$CA_AGENT_PID" 2>/dev/null ||
        true

      if [ "''${#CLEANUP_DIRS[@]}" -gt 0 ]; then
        for d in "''${CLEANUP_DIRS[@]}"; do
          rm -rf "$d"
        done
      fi
    }

    trap cleanup EXIT

    usage() {
      cat <<EOF
    Melinoe SSH Host Provisioning Tool

    Usage:
      mel-ssh-provision <command> [arguments]

    Commands:
      list
          List all hosts configured in the NixOS fleet.

      show <host>
          Show generated metadata and principals for a host.

      bootstrap <host>
          Generate host keys if necessary, sign them, and deploy the
          resulting certificate.

      rotate <host>
          Force regeneration of the target host keys, sign them, and
          redeploy the certificate.

      renew <host>
          Resign the existing host key and redeploy the certificate.

      renew-all
          Renew certificates for every configured host.
    EOF

      exit 1
    }

    require_host() {
      local host="''${1:-}"

      [ -z "$host" ] && {
        echo "ERROR: Missing required HOST argument." >&2
        exit 1
      }

      jq -e --arg h "$host" 'has($h)' <<< "$hosts_json" >/dev/null || {
        echo "ERROR: Unknown host '$host'." >&2
        exit 1
      }
    }

    get_field() {
      jq -r \
        --arg h "$1" \
        --arg fld "$2" \
        '.[$h][$fld]' <<< "$hosts_json"
    }

    get_principals() {
      jq -r \
        --arg h "$1" \
        '.[$h].principals | join(",")' <<< "$hosts_json"
    }

    setup_ca_agent() {
      echo "Unlocking CA key into ephemeral agent..." >&2

      local ca_dir
      ca_dir="$(mktemp -d)"
      CLEANUP_DIRS+=("$ca_dir")

      export SSH_AUTH_SOCK="$ca_dir/agent.sock"

      eval "$(ssh-agent -a "$SSH_AUTH_SOCK")"
      CA_AGENT_PID="''${SSH_AGENT_PID:-}"

      ssh-add "$ca_key" >/dev/null
    }

    process_host() {
      local host="$1"
      local force="$2"
      local target
      local principals
      local tmpdir
      local pubfile
      local certfile

      require_host "$host"

      target="$(get_field "$host" sshTarget)"
      principals="$(get_principals "$host")"

      echo "Processing $host ($target)..."

      tmpdir="$(mktemp -d)"
      CLEANUP_DIRS+=("$tmpdir")

      pubfile="$tmpdir/host.pub"
      certfile="$tmpdir/host-cert.pub"

      local ssh_agent_opt=()

      [ -n "$USER_SSH_AUTH_SOCK" ] &&
        ssh_agent_opt=(-o "IdentityAgent=$USER_SSH_AUTH_SOCK")

      # shellcheck disable=SC2029
      ssh "''${ssh_agent_opt[@]}" "root@$target" "bash -s" -- "$force" <<'EOF' > "$pubfile"
    set -euo pipefail

    key=/etc/ssh/ssh_host_ed25519_key
    pub=/etc/ssh/ssh_host_ed25519_key.pub

    if [ "$1" = "true" ] || [ ! -s "$pub" ]; then
      rm -f "$key" "$pub"
      ssh-keygen -t ed25519 -N "" -f "$key" -q
    fi

    cat "$pub"
    EOF

      ssh-keygen \
        -q \
        -U \
        -s "''${ca_key}.pub" \
        -I "$host" \
        -h \
        -n "$principals" \
        -V "$validity" \
        "$pubfile"

      [ ! -f "$certfile" ] && {
        echo "ERROR: certificate not generated: $certfile" >&2
        exit 1
      }

      # shellcheck disable=SC2029
      ssh "''${ssh_agent_opt[@]}" "root@$target" \
        "cat > '$host_cert_path' && systemctl reload sshd" \
        < "$certfile"

      echo "OK: $host updated successfully."
    }

    cmd="''${1:-}"
    shift || true

    case "$cmd" in
      list)
        jq -r 'keys[]' <<< "$hosts_json" | sort
        ;;

      show)
        require_host "''${1:-}"

        jq -r \
          --arg h "''${1:-}" \
          '.[$h]' <<< "$hosts_json"
        ;;

      bootstrap|rotate|renew)
        target_host="''${1:-}"
        require_host "$target_host"

        force_gen=false
        [ "$cmd" = "rotate" ] && force_gen=true

        setup_ca_agent
        process_host "$target_host" "$force_gen"
        ;;

      renew-all)
        setup_ca_agent

        failures=()

        for h in $(jq -r 'keys[]' <<< "$hosts_json"); do
          if ! process_host "$h" false; then
            echo "FAILED: $h" >&2
            failures+=("$h")
          fi
        done

        if [ "''${#failures[@]}" -gt 0 ]; then
          echo \
            -e "\nCompleted with ''${#failures[@]} failure(s): ''${failures[*]}" \
            >&2
          exit 1
        fi

        echo "All hosts renewed successfully."
        ;;

      *)
        usage
        ;;
    esac
  '';
}
