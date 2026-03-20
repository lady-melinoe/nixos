{ config, pkgs, lib, ... }:

let
  incusSnapshotScript = pkgs.writeShellScript "incus-container-snapshot" ''
    #!/usr/bin/env bash
    set -euo pipefail

    SRC="${config.melinoe.incusDefaultStorageSource}containers"
    DEST_BASE="/array/container-snapshots"
    TIMESTAMP=$(date +%Y%m%d-%H%M%S)

    mkdir -p "$DEST_BASE"

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
    """
    Parse a snapshot directory name formatted as YYYYMMDD-HHMMSS.

    Returns:
        int: UNIX timestamp (seconds) if the name matches the expected format
        None: if the name does not match the expected format

    This intentionally ignores any directory names that do not strictly
    follow the timestamp format. The snapshot generator produces exact-format
    names, so anything else is considered unrelated.
    """
    try:
        dt = datetime.strptime(name, TS_FORMAT)
        return int(dt.timestamp())
    except ValueError:
        return None


def ts(ts_value):
    """
    Convert a UNIX timestamp to YYYYMMDD-HHMMSS so the script can locate
    the corresponding snapshot directory.
    """
    dt = datetime.fromtimestamp(ts_value)
    return dt.strftime(TS_FORMAT)


def generate_intervals(now_ts):
    """
    Generate retention intervals as (start, end) UNIX timestamps.

    Intervals use half-open semantics:
        start <= t < end

    Rationale:
        • This ensures no snapshot can belong to two adjacent intervals.
        • This avoids boundary misclassification at exactly on-the-hour
          or on-the-day timestamps.
        • The pruning logic processes intervals from oldest to newest,
          but interval membership remains defined purely by timestamp.

    Structure:
        • A non-overlapping set of tiers covering the last 24 hours.
        • Additional longer-term tiers permitted to overlap by design
          (daily, weekly, monthly, yearly).

    Returns:
        List[(start_ts, end_ts)] ordered from oldest to newest.
    """
    intervals = []

    def add_intervals(count, step_sec, offset_sec=0):
        """
        Add 'count' intervals of fixed width 'step_sec', ending at offsets
        relative to 'now_ts'.

        Example:
            With 12 × 600-second intervals (10 minutes):
                [now-600, now)
                [now-1200, now-600)
                ...
        """
        for i in range(count):
            start = now_ts - offset_sec - (i + 1) * step_sec
            end   = now_ts - offset_sec - i * step_sec
            intervals.append((start, end))

    hour = 3600
    first_day_tiers = [
        (12,   10 * 60,     0),
        (12,   30 * 60,     2 * hour),
        (8,    3600,        8 * hour),
        (4,    2 * 3600,    16 * hour),
    ]
    for count, step, offset in first_day_tiers:
        add_intervals(count, step, offset)

    other_tiers = [
        (7,    86400,          0),
        (4,    7 * 86400,      0),
        (12,   28 * 86400,     0),
        (7,    364 * 86400,    0),
    ]
    for count, step, offset in other_tiers:
        add_intervals(count, step, offset)

    return intervals[::-1]


def prune_container(container_dir):
    """
    Apply retention pruning to the snapshot tree under 'container_dir'.

    Behavior:
        • Keep the newest snapshot within each retention interval.
        • When a gap occurs (interval with no snapshots), the next interval
          that contains snapshots keeps both:
              - the oldest (recovery marker)
              - the newest (normal retention)
        • Snapshots whose timestamps lie in the future are never deleted.
          These typically result from testing or date misconfiguration.

    Subvolume deletion uses 'btrfs subvolume delete', which is atomic.
    """
    cname = os.path.basename(container_dir)
    print(f"\n=== Pruning container: {cname} ===")

    raw_entries = os.listdir(container_dir)

    snapshots = []
    for e in raw_entries:
        t = parse_ts(e)
        if t is not None:
            snapshots.append(t)

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
        end_i   = bisect.bisect_left(snapshots, end)
        snaps   = snapshots[start_i:end_i]

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
    """
    Delete a Btrfs subvolume at 'path'.

    btrfs subvolume delete is an atomic operation. Any failure is printed
    for diagnostic purposes. Standard output is suppressed because normal
    deletion output is not needed in logs.
    """
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
    """
    Iterate over all container directories under BASE_DIR and prune each.
    """
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
