{
  pkgs,
  lib,
  nixosConfigurations,
}:

let
  deployDomain = "intra.melinoe.xyz";

  nodeNames = builtins.attrNames nixosConfigurations;

  knownNodesStr =
    lib.concatStringsSep ", " nodeNames;

  deployNodesJson = builtins.toJSON (
    lib.mapAttrs (
      name: _:
      "${name}.${deployDomain}"
    ) nixosConfigurations
  );
in
pkgs.writeShellApplication {
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
      host=$(echo "$tag" |
        sed -E 's/^\[DEPLOY[[:space:]]*//; s/\]$//')

      [ -z "$host" ] &&
        deploy_all=true ||
        target_names+=("$host")
    done < <(
      echo "$commit_msg" |
        grep -oP '\[DEPLOY(?:\s+[^\]]+)?\]' ||
        true
    )

    $deploy_all &&
      target_names=(${lib.concatStringsSep " " nodeNames})

    if [ "''${#target_names[@]}" -eq 0 ]; then
      echo "ERROR: No [DEPLOY] tags found in commit message." >&2
      exit 1
    fi

    for host in "''${target_names[@]}"; do
      if ! jq -e --arg h "$host" 'has($h)' <<< "$nodes_json" >/dev/null; then
        echo \
          "ERROR: Unknown node '$host'. Known nodes: ${knownNodesStr}" \
          >&2
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

    printf \
      '@cert-authority * %s\n' \
      "$(cat "$workdir/.ssh/host_ca.pub")" \
      > "$workdir/.ssh/known_hosts"

    SSH_OPTS=(
      -n
      -i "$workdir/.ssh/id_ed25519"
      -o "CertificateFile=$workdir/.ssh/id_ed25519-cert.pub"
      -o "UserKnownHostsFile=$workdir/.ssh/known_hosts"
      -o StrictHostKeyChecking=yes
      -o BatchMode=yes
    )

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

    echo \
      -e "\nDeployed ''${#target_names[@]} node(s), ''${#failed[@]} failure(s)."

    if [ "''${#failed[@]}" -gt 0 ]; then
      echo "Failed nodes: ''${failed[*]}" >&2
      exit 1
    fi
  '';
}
