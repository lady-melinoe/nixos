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
  deployDomain = "intra.melinoe.xyz";
  nodeNames = builtins.attrNames nixosConfigurations;
  knownNodesStr = lib.concatStringsSep ", " nodeNames;
  deployNodesJson = builtins.toJSON (
    lib.mapAttrs (name: _: "${name}.${deployDomain}") nixosConfigurations
  );
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
      USER_SSH_AUTH_SOCK="''${SSH_AUTH_SOCK:-}"
      CLEANUP_DIRS=()
      CA_AGENT_PID=""
      cleanup() {
        [ -n "$CA_AGENT_PID" ] && kill "$CA_AGENT_PID" 2>/dev/null || true
        if [ "''${#CLEANUP_DIRS[@]}" -gt 0 ]; then
          for d in "''${CLEANUP_DIRS[@]}"; do rm -rf "$d"; done
        fi
      }
      trap cleanup EXIT
      usage() {
        cat <<EOF
      Melinoe SSH Host CA Management Tool
      Usage: mel-ssh-host-ca <command> [arguments]
      Commands:
        list                List all hostnames configured in the NixOS fleet.
        show <host>         Show the generated deployment metadata and principals for a host.
        bootstrap <host>    Fetch/generate host keys, sign them, and deploy them to a new host.
        rotate <host>       Force regeneration of target host keys, resign, and redeploy them.
        renew <host>        Resign the current host key and redeploy the new certificate.
        renew-all           Batch renew all hosts configured in the NixOS metadata.
      EOF
        exit 1
      }
      require_host() {
        local host="''${1:-}"
        [ -z "$host" ] && { echo "ERROR: Missing required HOST argument." >&2; exit 1; }
        jq -e --arg h "$host" 'has($h)' <<< "$hosts_json" >/dev/null || { echo "ERROR: Unknown host '$host'." >&2; exit 1; }
      }
      get_field() { jq -r --arg h "$1" --arg fld "$2" '.[$h][$fld]' <<< "$hosts_json"; }
      get_principals() { jq -r --arg h "$1" '.[$h].principals | join(",")' <<< "$hosts_json"; }
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
        local host="$1" force="$2" target principals tmpdir pubfile certfile
        require_host "$host"
        target="$(get_field "$host" sshTarget)"
        principals="$(get_principals "$host")"
        echo "Processing $host ($target)..."
        tmpdir="$(mktemp -d)"
        CLEANUP_DIRS+=("$tmpdir")
        pubfile="$tmpdir/host.pub"
        certfile="$tmpdir/host-cert.pub"
        local ssh_agent_opt=()
        [ -n "$USER_SSH_AUTH_SOCK" ] && ssh_agent_opt=(-o "IdentityAgent=$USER_SSH_AUTH_SOCK")
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
        ssh-keygen -q -U -s "''${ca_key}.pub" -I "$host" -h -n "$principals" -V "$validity" "$pubfile"
        [ ! -f "$certfile" ] && { echo "ERROR: certificate not generated: $certfile" >&2; exit 1; }
        # shellcheck disable=SC2029
        ssh "''${ssh_agent_opt[@]}" "root@$target" "cat > '$host_cert_path' && systemctl reload sshd" < "$certfile"
        echo "OK: $host updated successfully."
      }
      cmd="''${1:-}"
      shift || true
      case "$cmd" in
        list) jq -r 'keys[]' <<< "$hosts_json" | sort ;;
        show)
          require_host "''${1:-}"
          jq -r --arg h "''${1:-}" '.[$h]' <<< "$hosts_json"
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
            echo -e "\nCompleted with ''${#failures[@]} failure(s): ''${failures[*]}" >&2
            exit 1
          fi
          echo "All hosts renewed successfully."
          ;;
        *) usage ;;
      esac
    '';
  };
  mel-deploy = pkgs.writeShellApplication {
    name = "mel-deploy";
    runtimeInputs = with pkgs; [
      coreutils
      gnugrep
      jq
      openssh
    ];
    text = ''
      set -euo pipefail
      nodes_json='${deployNodesJson}'
      usage() {
        cat >&2 <<EOF
      Melinoe deployment helper — drives update-safe over SSH.
      Usage: mel-deploy <commit-message>
        [DEPLOY]         Deploy every node in the fleet.
        [DEPLOY <host>]  Deploy the named node only.
      Known nodes: ${knownNodesStr}
      EOF
        exit 1
      }
      commit_msg="''${1:-}"
      [ -z "$commit_msg" ] && usage
      deploy_all=false
      declare -a target_names=()
      while IFS= read -r tag; do
        host=$(echo "$tag" | sed -E 's/^\[DEPLOY[[:space:]]*//; s/\]$//')
        [ -z "$host" ] && deploy_all=true || target_names+=("$host")
      done < <(echo "$commit_msg" | grep -oP '\[DEPLOY(?:\s+[^\]]+)?\]' || true)
      $deploy_all && target_names=(${lib.concatStringsSep " " nodeNames})
      if [ "''${#target_names[@]}" -eq 0 ]; then
        echo "ERROR: No [DEPLOY] tags found in commit message." >&2
        exit 1
      fi
      for host in "''${target_names[@]}"; do
        if ! jq -e --arg h "$host" 'has($h)' <<< "$nodes_json" >/dev/null; then
          echo "ERROR: Unknown node '$host'. Known nodes: ${knownNodesStr}" >&2
          exit 1
        fi
      done
      workdir="$(mktemp -d)"
      trap 'rm -rf "$workdir"' EXIT
      mkdir -p "$workdir/.ssh"
      # shellcheck disable=SC2154
      base64 -d < "$ssh_key" > "$workdir/.ssh/id_ed25519"
      # shellcheck disable=SC2154
      base64 -d < "$ssh_cert" > "$workdir/.ssh/id_ed25519-cert.pub"
      # shellcheck disable=SC2154
      base64 -d < "$ssh_host_ca" > "$workdir/.ssh/host_ca.pub"
      chmod 600 "$workdir/.ssh/id_ed25519"
      printf '@cert-authority * %s\n' "$(cat "$workdir/.ssh/host_ca.pub")" > "$workdir/.ssh/known_hosts"
      SSH_OPTS=(-n -i "$workdir/.ssh/id_ed25519" -o "CertificateFile=$workdir/.ssh/id_ed25519-cert.pub" -o "UserKnownHostsFile=$workdir/.ssh/known_hosts" -o StrictHostKeyChecking=yes -o BatchMode=yes)
      declare -a failed=()
      for host in "''${target_names[@]}"; do
        fqdn=$(jq -r --arg h "$host" '.[$h]' <<< "$nodes_json")
        echo "Deploying: $host → $fqdn"
        # shellcheck disable=SC2029
        if ssh "''${SSH_OPTS[@]}" "gitlab-deploy@$fqdn"; then
          echo "OK: $host"
        else
          echo "FAILED: $host" >&2
          failed+=("$host")
        fi
      done
      echo -e "\nDeployed ''${#target_names[@]} node(s), ''${#failed[@]} failure(s)."
      if [ "''${#failed[@]}" -gt 0 ]; then
        echo "Failed nodes: ''${failed[*]}" >&2
        exit 1
      fi
    '';
  };
}
