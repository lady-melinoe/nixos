{
  pkgs,
  lib,
  nixosConfigurations,
}:

let
  # Domain used for deployment SSH connections (intra = internal reachability).
  # This matches the CI usage of <host>.intra.melinoe.xyz, distinct from
  # <host>.infra.melinoe.xyz which is used for WireGuard peer endpoints.
  deployDomain = "intra.melinoe.xyz";

  # Bake the name → FQDN map into the derivation at eval time so the script
  # needs no network or flake access at runtime.
  deployNodesJson = builtins.toJSON (
    lib.mapAttrs (name: _: "${name}.${deployDomain}") nixosConfigurations
  );

in
{
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

      # Node map baked in at build time: { "hecate": "hecate.intra.melinoe.xyz", … }
      nodes_json='${deployNodesJson}'

      # ── Usage ──────────────────────────────────────────────────────────────
      usage() {
        cat >&2 <<EOF
      Melinoe deployment helper — drives update-safe over SSH.

      Usage:
        mel-deploy <commit-message>

      Parses [DEPLOY] and [DEPLOY <host>] tags from the commit message.

        [DEPLOY]            Deploy every node in the fleet.
        [DEPLOY <host>]     Deploy the named node only.

      Multiple tags are additive; a bare [DEPLOY] overrides all named tags.

      SSH credentials are read from CI file-type variables (paths):
        \$ssh_key       Path to base64-encoded private key file
        \$ssh_cert      Path to base64-encoded certificate file
        \$ssh_host_ca   Path to base64-encoded host CA public key file

      Known nodes: $(printf '%s\n' "$nodes_json" | jq -r '[keys[]] | join(", ")')
      EOF
        exit 1
      }

      commit_msg="''${1:-}"
      [ -z "$commit_msg" ] && usage

      # ── Parse [DEPLOY] / [DEPLOY <host>] tags ──────────────────────────────
      deploy_all=false
      declare -a target_names=()

      while IFS= read -r tag; do
        # Strip surrounding brackets and optional "DEPLOY" prefix
        inner="''${tag#[}"
        inner="''${inner%]}"
        # inner is now "DEPLOY" or "DEPLOY <host>"
        host="''${inner#DEPLOY}"
        host="''${host# }"   # strip leading space if present

        if [ -z "$host" ]; then
          deploy_all=true
        else
          target_names+=("$host")
        fi
      done < <(printf '%s' "$commit_msg" | grep -oP '\[DEPLOY(?:\s+[^\]]+)?\]' || true)

      if $deploy_all; then
        # Expand to every node; discard any individually named ones
        mapfile -t target_names < <(printf '%s\n' "$nodes_json" | jq -r 'keys[]' | sort)
      fi

      if [ "''${#target_names[@]}" -eq 0 ]; then
        echo "ERROR: No [DEPLOY] tags found in commit message." >&2
        echo "       Commit message was: $commit_msg" >&2
        exit 1
      fi

      # ── Validate all requested hosts before touching anything ───────────────
      for host in "''${target_names[@]}"; do
        if ! printf '%s\n' "$nodes_json" | jq -e --arg h "$host" 'has($h)' >/dev/null 2>&1; then
          echo "ERROR: Unknown node '$host'." >&2
          echo "       Known nodes: $(printf '%s\n' "$nodes_json" | jq -r '[keys[]] | join(", ")')" >&2
          exit 1
        fi
      done

      # ── Set up SSH credentials ──────────────────────────────────────────────
      # Expects GitLab CI file-type variables: $ssh_key, $ssh_cert, $ssh_host_ca
      # Each file contains the base64-encoded credential.
      workdir="$(mktemp -d)"
      trap 'rm -rf "$workdir"' EXIT

      mkdir -p "$workdir/.ssh"

      base64 -d < "$ssh_key"     > "$workdir/.ssh/id_ed25519"
      base64 -d < "$ssh_cert"    > "$workdir/.ssh/id_ed25519-cert.pub"
      base64 -d < "$ssh_host_ca" > "$workdir/.ssh/host_ca.pub"

      chmod 600 "$workdir/.ssh/id_ed25519"

      {
        printf '@cert-authority * '
        cat "$workdir/.ssh/host_ca.pub"
      } > "$workdir/.ssh/known_hosts"

      SSH_OPTS=(
        -n
        -i "$workdir/.ssh/id_ed25519"
        -o "CertificateFile=$workdir/.ssh/id_ed25519-cert.pub"
        -o "UserKnownHostsFile=$workdir/.ssh/known_hosts"
        -o StrictHostKeyChecking=yes
        -o BatchMode=yes
      )

      # ── Deploy ─────────────────────────────────────────────────────────────
      declare -a failed=()

      for host in "''${target_names[@]}"; do
        fqdn="$(printf '%s\n' "$nodes_json" | jq -r --arg h "$host" '.[$h]')"
        echo "Deploying: $host → $fqdn"

        if ssh "''${SSH_OPTS[@]}" "root@$fqdn"; then
          echo "OK: $host"
        else
          echo "FAILED: $host" >&2
          failed+=("$host")
        fi
      done

      # ── Summary ────────────────────────────────────────────────────────────
      echo ""
      echo "Deployed ''${#target_names[@]} node(s), ''${#failed[@]} failure(s)."

      if [ "''${#failed[@]}" -gt 0 ]; then
        echo "Failed nodes: ''${failed[*]}" >&2
        exit 1
      fi
    '';
  };
}
