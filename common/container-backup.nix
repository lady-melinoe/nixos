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

  pruneScript = pkgs.writeTextFile {
    name = "prune-container-snapshots";
    destination = "/bin/prune-container-snapshots";
    executable = true;
    text = ''#!/usr/bin/env python3
import os
import bisect
import subprocess
from datetime import datetime

BASE_DIR = "/array/container-snapshots"
TS_FORMAT = "%Y%m%d-%H%M%S"


def parse_ts(name):
    try:
        dt = datetime.strptime(name, TS_FORMAT)
        return int(dt.timestamp())
    except ValueError:
        return None


def ts(ts_value):
    dt = datetime.fromtimestamp(ts_value)
    return dt.strftime(TS_FORMAT)


def generate_intervals(now_ts):
    intervals = []

    def add_intervals(count, step_sec, offset_sec=0):
        for i in range(count):
            start = now_ts - offset_sec - (i + 1) * step_sec
            end = now_ts - offset_sec - i * step_sec
            intervals.append((start, end))

    tiers = [
        (12, 10 * 60, 0),
        (12, 30 * 60, 2 * 3600),
        (8, 3600, 8 * 3600),
        (4, 2 * 3600, 16 * 3600),
        (7, 86400, 0),
        (4, 7 * 86400, 0),
        (12, 28 * 86400, 0),
        (7, 364 * 86400, 0),
    ]

    for count, step, offset in tiers:
        add_intervals(count, step, offset)

    return intervals[::-1]


def prune_container(container_dir):
    cname = os.path.basename(container_dir)
    print(f"\n=== Pruning container: {cname} ===")

    raw_entries = os.listdir(container_dir)
    snapshots = [parse_ts(e) for e in raw_entries if parse_ts(e) is not None]
    snapshots.sort()
    print(f"Found {len(snapshots)} snapshots.")
    if not snapshots:
        return

    now_ts = int(datetime.now().timestamp())
    keep = set()

    future = {s for s in snapshots if s > now_ts}
    if future:
        print(f"Found {len(future)} future snapshots — keeping all.")
    keep |= future

    intervals = generate_intervals(now_ts)
    gap_open = False

    for start, end in intervals:
        start_i = bisect.bisect_left(snapshots, start)
        end_i = bisect.bisect_right(snapshots, end)
        snaps = snapshots[start_i:end_i]

        if not snaps:
            gap_open = True
            continue

        newest = snaps[-1]
        keep.add(newest)

        if gap_open and len(snaps) > 1:
            keep.add(snaps[0])

        gap_open = False

    to_delete = [s for s in snapshots if s not in keep]
    print(f"Keeping {len(keep)}, deleting {len(to_delete)}")

    for s in sorted(to_delete):
        if s in future:
            continue
        timestamp_dir = os.path.join(container_dir, ts(s))
        subvolume_dir = os.path.join(timestamp_dir, cname)
        print(f"DELETE: {subvolume_dir}")

        delete_subvolume(subvolume_dir)

        try:
            os.rmdir(timestamp_dir)
            print(f"Removed empty timestamp directory: {timestamp_dir}")
        except OSError as e:
            print(f"Failed to remove timestamp directory {timestamp_dir}: {e}")

    print("Done.")


def delete_subvolume(path):
    try:
        subprocess.run(
            ["btrfs", "subvolume", "delete", path],
            check=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        print(f"Deleted subvolume: {path}")
    except subprocess.CalledProcessError as e:
        print(f"Error deleting subvolume {path}: {e.stderr.decode()}")


def main():
    for c in os.listdir(BASE_DIR):
        cdir = os.path.join(BASE_DIR, c)
        if os.path.isdir(cdir):
            prune_container(cdir)
    print("\nAll pruning completed.")


if __name__ == "__main__":
    main()
''; };
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

  systemd.services.melinoe-incus-snapshot-prune = {
    description = "Prune Incus container snapshots";
    serviceConfig = {
      Type = "oneshot";
      ExecStart = "${pruneScript}/bin/prune-container-snapshots";
    };
    path = [
      pkgs.python3
      pkgs.btrfs-progs
    ];
  };

  systemd.timers.melinoe-incus-snapshot = {
    wantedBy = [ "timers.target" ];
    timerConfig = {
      OnCalendar = "*:0/10";
      Persistent = true;
    };
  };

  systemd.timers.melinoe-incus-snapshot-prune = {
    wantedBy = [ "timers.target" ];
    timerConfig = {
      OnCalendar = "*:0/5";
      Persistent = true;
    };
  };
}
