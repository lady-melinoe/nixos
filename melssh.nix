{ pkgs, lib, nixosConfigurations }:

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

      # Preserve the user's original SSH agent for outbound host connections
      USER_SSH_AUTH_SOCK="''${SSH_AUTH_SOCK:-}"

      # Centralized cleanup. Anything added to CLEANUP_DIRS, and the CA
      # agent PID (if any), gets torn down on EXIT -- including error exits
      # caused by `set -e`, which a `trap ... RETURN` would NOT catch.
      CLEANUP_DIRS=()
      CA_AGENT_PID=""

      cleanup() {
        if [ -n "$CA_AGENT_PID" ]; then
          kill "$CA_AGENT_PID" 2>/dev/null || true
        fi
        if [ "''${#CLEANUP_DIRS[@]}" -gt 0 ]; then
          for d in "''${CLEANUP_DIRS[@]}"; do
            rm -rf "$d"
          done
        fi
      }
      trap cleanup EXIT

      usage() {
        cat <<EOF
Melinoe SSH Host CA Management Tool

Usage: 
  mel-ssh-host-ca <command> [arguments]

Commands:
  list                 List all hostnames configured in the NixOS fleet.
  show <host>          Show the generated deployment metadata and principals for a host.
  bootstrap <host>     Fetch/generate host keys, sign them, and deploy them to a new host.
  rotate <host>        Force regeneration of target host keys, resign, and redeploy them.
  renew <host>         Resign the current host key and redeploy the new certificate.
  renew-all            Batch renew all hosts configured in the NixOS metadata.
                        Continues past per-host failures and reports a summary at the end.

Environment Variables (Optional):
  MEL_HOST_CA_KEY      Path to private CA key (Default: ~/ssh-ca-keys/ssh-host-ca)
  MEL_HOST_CERT_PATH   Remote deployment path (Default: /etc/ssh/ssh_host_ed25519_key-cert.pub)
  MEL_HOST_CERT_VALIDITY Certificate duration lifespans (Default: +30d)

EOF
        exit 1
      }

      require_host() {
        local host="''${1:-}"
        if [ -z "$host" ]; then
          echo "ERROR: Missing required HOST argument." >&2
          echo "Run 'mel-ssh-host-ca list' to see available hosts." >&2
          exit 1
        fi
        printf '%s\n' "$hosts_json" | jq -e --arg h "$host" 'has($h)' >/dev/null \
          || { echo "ERROR: Unknown host '$host'. Run 'mel-ssh-host-ca list' for valid targets." >&2; exit 1; }
      }

      get_field() {
        # Bracket notation + jq vars, not bash-interpolated dot notation:
        # hostnames can contain '-', which dot notation can't parse, and
        # plain "$h" here would be a bash var (unset outside renew-all),
        # not jq's --arg, under double quotes.
        printf '%s\n' "$hosts_json" | jq -r --arg h "$1" --arg fld "$2" '.[$h][$fld]'
      }

      get_principals() {
        printf '%s\n' "$hosts_json" | jq -r --arg h "$1" '.[$h].principals | join(",")'
      }

      setup_ca_agent() {
        echo "Unlocking CA key into ephemeral agent..." >&2

        local ca_dir
        ca_dir="$(mktemp -d)"
        CLEANUP_DIRS+=("$ca_dir")
        export SSH_AUTH_SOCK="$ca_dir/agent.sock"

        # Start agent and parse PID to ensure clean teardown
        eval "$(ssh-agent -a "$SSH_AUTH_SOCK")"
        CA_AGENT_PID="''${SSH_AGENT_PID:-}"

        ssh-add "$ca_key" >/dev/null
      }

      process_host() {
        local host="$1"
        local force="$2"

        require_host "$host"

        local target
        target="$(get_field "$host" sshTarget)"

        local principals
        principals="$(get_principals "$host")"

        echo "Processing $host ($target)..."

        local tmpdir
        tmpdir="$(mktemp -d)"
        CLEANUP_DIRS+=("$tmpdir")

        local pubfile="$tmpdir/host.pub"
        local certfile="$tmpdir/host-cert.pub"

        # Build custom SSH identity agent argument if original agent existed
        local ssh_agent_opt=()
        if [ -n "$USER_SSH_AUTH_SOCK" ]; then
          ssh_agent_opt=(-o "IdentityAgent=$USER_SSH_AUTH_SOCK")
        fi

        # --- fetch or generate host key on remote (Uses normal user SSH agent) ---
# shellcheck disable=SC2029
        ssh "''${ssh_agent_opt[@]}" "root@$target" "bash -s" -- "$force" <<'EOF' > "$pubfile"
set -euo pipefail
force="$1"

key=/etc/ssh/ssh_host_ed25519_key
pub=/etc/ssh/ssh_host_ed25519_key.pub

if [ "$force" = "true" ] || [ ! -s "$pub" ]; then
  rm -f "$key" "$pub"
  ssh-keygen -t ed25519 -N "" -f "$key" -q
fi

cat "$pub"
EOF

        # --- sign with CA (Uses ephemeral CA agent via -U and current SSH_AUTH_SOCK) ---
        ssh-keygen -q -U -s "''${ca_key}.pub" \
          -I "$host" \
          -h \
          -n "$principals" \
          -V "$validity" \
          "$pubfile"

        # ssh-keygen outputs cert next to input file (e.g. host.pub -> host-cert.pub)
        if [ ! -f "$certfile" ]; then
          echo "ERROR: certificate not generated: $certfile" >&2
          exit 1
        fi

        # --- deploy cert (Uses normal user SSH agent) ---
# shellcheck disable=SC2029
        ssh "''${ssh_agent_opt[@]}" "root@$target" "cat > '$host_cert_path' && systemctl reload sshd" \
          < "$certfile"

        echo "OK: $host updated successfully."
      }

      cmd="''${1:-}"
      shift || true

      case "$cmd" in
        list)
          printf '%s\n' "$hosts_json" | jq -r 'keys[]' | sort
          ;;

        show)
          target_host="''${1:-}"
          require_host "$target_host"
          printf '%s\n' "$hosts_json" | jq -r --arg h "$target_host" '.[$h]'
          ;;

        bootstrap|rotate|renew)
          target_host="''${1:-}"
          require_host "$target_host"
          
          # Map operations to force values
          force_gen=false
          if [ "$cmd" = "rotate" ]; then
            force_gen=true
          fi

          setup_ca_agent
          process_host "$target_host" "$force_gen"
          ;;

        renew-all)
          setup_ca_agent
          failures=()
          for h in $(printf '%s\n' "$hosts_json" | jq -r 'keys[]'); do
            # `if ! process_host` suspends errexit for this call only, so one
            # bad host doesn't abort the whole batch -- the host's own error
            # message still prints, we just keep going afterward.
            if ! process_host "$h" false; then
              echo "FAILED: $h" >&2
              failures+=("$h")
            fi
          done
          if [ "''${#failures[@]}" -gt 0 ]; then
            echo "" >&2
            echo "Completed with ''${#failures[@]} failure(s): ''${failures[*]}" >&2
            exit 1
          fi
          echo "All hosts renewed successfully."
          ;;

        *)
          usage
          ;;
      esac
    '';
  };
}
