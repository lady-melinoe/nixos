{
  config,
  pkgs,
  lib,
  inputs,
  ...
}:

let
  incusSnapshotScript = pkgs.writeTextFile {
    name = "incus-container-snapshot";
    destination = "/bin/incus-container-snapshot";
    executable = true;
    text = ''
      #!/usr/bin/env python3
      import subprocess
      import threading
      import time
      from datetime import datetime
      from pathlib import Path

      SRC = Path("/array/incus/containers")
      DEST_BASE = Path("/array/container-snapshots")
      REPO_BASE = Path("/array/borg")
      INTERVAL_SECONDS = 30 * 60


      def should_skip(name: str) -> bool:
          return (
              name.startswith("migration.")
              or name.endswith("-old")
              or name.endswith("-transfer")
          )


      def ensure_repo(repo_path: Path, container_name: str) -> None:
          if not repo_path.exists():
              print(f"No borg repo for {container_name}, creating one")
              subprocess.run(
                  ["borg", "repo-create", "--repo", str(repo_path), "--encryption=none"],
                  check=True,
              )


      def ingest_snapshot(container_name: str, repo_path: Path, dest: Path) -> bool:
          timestamp = dest.name
          print(f"  -> ingesting {timestamp} into borg")
          result = subprocess.run(
              [
                  "borg",
                  "create",
                  "--repo",
                  str(repo_path),
                  "--stats",
                  "--numeric-ids",
                  "--files-changed",
                  "disabled",
                  "--compression",
                  "none",
                  "--timestamp",
                  str(dest),
                  timestamp,
                  f"{dest}/./{container_name}",
              ]
          )
          if result.returncode != 0:
              print(f"ERROR: {container_name}/{timestamp} failed", flush=True)
              return False

          subprocess.run(["btrfs", "subvolume", "delete", str(dest / container_name)], check=True)
          subprocess.run(["rm", "-rf", str(dest)], check=True)
          print(f"  -> ingested and removed {container_name}/{timestamp}")
          return True


      def ingest_pending_snapshots(container_name: str, repo_path: Path) -> None:
          container_dir = DEST_BASE / container_name
          if not container_dir.exists():
              return

          had_failure = False
          for dest in sorted(container_dir.iterdir()):
              if not dest.is_dir():
                  continue
              if not (dest / container_name).exists():
                  continue
              if not ingest_snapshot(container_name, repo_path, dest):
                  had_failure = True

          if had_failure:
              print(f"One or more snapshots for {container_name} failed and will be retried next cycle")


      def snapshot_container(container_path: Path, timestamp: str) -> None:
          container_name = container_path.name
          if should_skip(container_name):
              print(f"Skipping {container_name}")
              return

          if subprocess.run(
              ["btrfs", "subvolume", "show", str(container_path)],
              stdout=subprocess.DEVNULL,
              stderr=subprocess.DEVNULL,
          ).returncode != 0:
              print(f"Skipping {container_name} (not a Btrfs subvolume)")
              return

          repo_path = REPO_BASE / container_name
          ensure_repo(repo_path, container_name)

          dest = DEST_BASE / container_name / timestamp
          dest.mkdir(parents=True, exist_ok=True)
          subprocess.run(
              ["btrfs", "subvolume", "snapshot", "-r", str(container_path), str(dest)],
              check=True,
          )
          print(f"Snapshot created for {container_name}")

          ingest_pending_snapshots(container_name, repo_path)
          ingest_snapshot(container_name, repo_path, dest)


      def run_once() -> None:
          timestamp = datetime.now().strftime("%Y%m%d-%H%M%S")
          DEST_BASE.mkdir(parents=True, exist_ok=True)

          threads = []
          for entry in SRC.iterdir():
              if not entry.is_dir():
                  continue
              thread = threading.Thread(target=snapshot_container, args=(entry, timestamp))
              thread.start()
              threads.append(thread)

          for thread in threads:
              thread.join()

          print("Snapshots completed.")


      def main() -> None:
          while True:
              start = time.monotonic()
              run_once()
              elapsed = time.monotonic() - start
              sleep_for = max(0, INTERVAL_SECONDS - elapsed)
              time.sleep(sleep_for)


      if __name__ == "__main__":
          main()
    '';
  };

in
{
  systemd.services.melinoe-incus-snapshot = {
    description = "Continuously create BTRFS snapshots of Incus containers";
    serviceConfig = {
      Type = "simple";
      ExecStart = "${incusSnapshotScript}/bin/incus-container-snapshot";
      Restart = "always";
      RestartSec = "5s";
    };
    path = [
      pkgs.btrfs-progs
      pkgs.python3
      inputs.self.packages.${pkgs.stdenv.hostPlatform.system}.borg-beta
    ];
  };
}
