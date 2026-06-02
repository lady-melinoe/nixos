{ pkgs, ... }:
let
  update = pkgs.writeShellScriptBin "update" ''
        #!/usr/bin/env bash
        set -euo pipefail

        no_pull=0
        no_verify=0
        rebuild_mode="switch"
        hostname=""

        usage() {
          cat <<'EOF'
    Usage: update [--no-pull] [--no-verify] [--boot] [--hostname HOSTNAME]

    Options:
      --no-pull         Skip git pull in /etc/nixos
      --no-verify       Skip verification of the fetched commit signature
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
            --no-verify)
              no_verify=1
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
          repo=/etc/nixos
          allowed_signers="$repo/allowed_signers"
          upstream="$(git -C "$repo" rev-parse --abbrev-ref --symbolic-full-name '@{u}')"

          git -C "$repo" fetch
          upstream_commit="$(git -C "$repo" rev-parse --verify "$upstream^{commit}")"

          if [ "$no_verify" -eq 0 ]; then
            git -C "$repo" -c "gpg.ssh.allowedSignersFile=$allowed_signers" verify-commit "$upstream_commit"
          fi

          git -C "$repo" merge --ff-only "$upstream_commit"
        fi

        exec nixos-rebuild "$rebuild_mode" --flake "/etc/nixos/#''${hostname}"
  '';

  moveScript = pkgs.writeShellScriptBin "move.sh" ''
    #!/usr/bin/env bash
    set -euo pipefail

    if [ -z "''${1:-}" ] && [ -z "''${2:-}" ]; then
      echo "Usage: $0 <source-container> <target-host>"
      echo "!!! Error: Missing both source and target arguments."
      exit 1
    fi

    if [ -z "''${1:-}" ]; then
      echo "Usage: $0 <source-container> <target-host>"
      echo "!!! Error: Missing source container argument."
      exit 1
    fi

    if [ -z "''${2:-}" ]; then
      echo "Usage: $0 <source-container> <target-host>"
      echo "!!! Error: Missing target host argument."
      exit 1
    fi

    src="$1"
    tgt="$2"

    if [[ "$(incus list --format json | jq -r --arg src "$src" 'first(.[] | select(.name == $src)) | .location')" == "$tgt" ]]; then
      echo "!!! Instance is already on target, aborting"
      exit 1
    fi

    status="$(incus list --format json | jq -r --arg src "$src" 'first(.[] | select(.name == $src)) | .status')"

    if [[ "$status" == "Stopped" ]]; then
      echo "=== Instance is stopped, performing direct move ==="
      if incus move "$src" --target "$tgt"; then
        echo "=== Move completed successfully ==="
        exit 0
      else
        echo "!!! Move failed"
        exit 1
      fi
    fi

    transfer="''${src}-transfer"
    snapshot="pre-transfer"
    old="''${src}-old"

    echo "=== Snapshotting Instance ==="
    if ! incus snapshot create "$src" "$snapshot"; then
      echo "!!! Snapshot failed, aborting."
      exit 1
    fi

    echo "=== Sending Pre-Shutdown Copy ==="
    if ! incus copy "$src" "$transfer" --target "$tgt" --refresh; then
      echo "!!! Pre-shutdown copy failed, cleaning up snapshot."
      incus snapshot delete "$src" "$snapshot" || true
      exit 1
    fi

    echo "=== Stopping Instance ==="
    if ! incus stop "$src"; then
      echo "!!! Stop failed, cleaning up pre-shutdown copy and snapshot."
      incus delete -f "$transfer" || true
      incus snapshot delete "$src" "$snapshot" || true
      exit 1
    fi

    echo "=== Sending Post-Shutdown Copy ==="
    if ! incus copy "$src" "$transfer" --target "$tgt" --refresh; then
      echo "!!! Post-shutdown copy failed, restoring original instance."
      incus delete -f "$transfer" || true
      incus snapshot delete "$src" "$snapshot" || true
      incus start "$src" || echo "!!! Failed to restart original instance."
      exit 1
    fi

    echo "=== Renaming Instances ==="
    if ! incus rename "$src" "$old"; then
      echo "!!! Rename $src -> $old failed, restoring original instance."
      incus delete -f "$transfer" || true
      incus snapshot delete "$src" "$snapshot" || true
      incus start "$src" || echo "!!! Failed to restart original instance."
      exit 1
    fi

    if ! incus rename "$transfer" "$src"; then
      echo "!!! Rename $transfer -> $src failed, rolling back."
      incus rename "$old" "$src" || echo "!!! Failed to rename $old back to $src"
      incus delete -f "$transfer" || true
      incus snapshot delete "$src" "$snapshot" || true
      incus start "$src" || echo "!!! Failed to restart original instance."
      exit 1
    fi

    echo "=== Starting Instance with retry loop (every 3s, max 15s) ==="
    start_time=$(date +%s)
    while true; do
      if incus start "$src"; then
        echo "=== Instance started successfully ==="
        incus delete -f "$old" || true
        incus snapshot delete "$src" "$snapshot" || true
        break
      fi

      now=$(date +%s)
      elapsed=$(( now - start_time ))
      if [ "$elapsed" -ge 15 ]; then
        echo "=== Giving up: start attempts exceeded 15s, rolling back ==="
        incus delete -f "$transfer" 2>/dev/null || true
        incus snapshot delete "$src" "$snapshot" 2>/dev/null || true
        incus rename "$old" "$src" 2>/dev/null || echo "!!! Failed to rename $old back to $src"
        incus start "$src" 2>/dev/null || echo "!!! Failed to start original container $src"
        exit 1
      fi

      sleep 3
    done

    echo "=== Migration Script Finished ==="
  '';
in
{
  environment.systemPackages = [
    update
    moveScript
  ];
}
