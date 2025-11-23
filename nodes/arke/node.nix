{ config, pkgs, lib, inputs, modulesPath, ... }:

let
  incusSnapshotScript = pkgs.writeShellScript "incus-container-snapshot" ''
    #!/usr/bin/env bash
    set -euo pipefail

    SRC="/array/incus/containers"
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
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    inputs.disko.nixosModules.disko
    ../../common/node-opts.nix
    ../../common/overlay.nix
    ../../common/underlay.nix
    ../../common/settings.nix
    ../../common/firewall.nix
    ../../common/users.nix
    ./disk-config.nix
  ];

  boot.loader.grub = {
    efiSupport = true;
    configurationLimit = 20;
    efiInstallAsRemovable = true;
    device = "nodev";
  };
  boot.loader.efi.efiSysMountPoint = "/boot/efi";
  boot.initrd.availableKernelModules = [ "ata_piix" "uhci_hcd" "xen_blkfront" "vmw_pvscsi" ];
  boot.initrd.kernelModules = [ "nvme" ];

  networking.hostName = "arke";
  networking.domain = "";
  networking.useDHCP = false;
  networking.interfaces.ens18.useDHCP = true;
  networking.interfaces.ens19 = {
    useDHCP = false;
    ipv4.addresses = [{
      address = "198.19.0.${toString config.melinoe.nodeId}";
      prefixLength = 24;
    }];
  };

  systemd.oomd.enable = false;

  services.qemuGuest.enable = true;

  melinoe.inetIfs = "ens18";
  melinoe.p2pIfs = "ens19";
  melinoe.nodeId = 2;
  melinoe.bgpPeers = [
    { id = 3; addr = "198.19.0.3"; }
    { id = 6; addr = "198.19.0.6"; }
    { id = 7; addr = "198.19.0.7"; }
  ];
  melinoe.incusDefaultStorageSource = "/array/incus/";

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
