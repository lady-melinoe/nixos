{ config, pkgs, lib, ... }:

let
  incusSnapshotScript = pkgs.writeShellScript "incus-container-snapshot" ''
    #!/usr/bin/env bash
    set -euo pipefail

    SRC="${config.melinoe.incusDefaultStorageSource}containers"
    DEST_BASE="/array/container-snapshots"
    TIMESTAMP=$(date +%Y%m%d-%H%M%S)

    for sv in "$SRC"/*; do
        [ -d "$sv" ] || continue
        CONTAINER_NAME=$(basename "$sv")
        if [[ "$CONTAINER_NAME" == migration.* ]]; then
            echo "Skipping $CONTAINER_NAME"
            continue
        fi
        if ! sudo btrfs subvolume show "$sv" &>/dev/null; then
            echo "Skipping $CONTAINER_NAME (not a Btrfs subvolume)"
            continue
        fi
        DEST="$DEST_BASE/$CONTAINER_NAME/$TIMESTAMP"
        mkdir -p "$DEST"
        sudo btrfs subvolume snapshot -r "$sv" "$DEST"
    done

    echo "Snapshots completed."
  '';
in {
  assertions = [
    {
      assertion = config.melinoe.incusDefaultStorageSource == "/array/incus/";
      message = "common/container-backup.nix requires melinoe.incusDefaultStorageSource to be /array/incus/ for now.";
    }
  ];

  systemd.services.melinoe-incus-snapshot = {
    description = "Create hourly Btrfs snapshots of Incus containers";
    serviceConfig = {
      Type = "oneshot";
      ExecStart = incusSnapshotScript;
    };
    path = [
      pkgs.coreutils
      pkgs.bash
      pkgs.btrfs-progs
      pkgs.sudo
    ];
  };

  systemd.timers.melinoe-incus-snapshot = {
    wantedBy = [ "timers.target" ];
    timerConfig = {
      OnCalendar = "hourly";
      Persistent = true;
    };
  };
}
