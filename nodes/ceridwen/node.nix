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
    inputs.disko.nixosModules.disko
    ../../common/node-opts.nix
    ../../common/overlay.nix
    ../../common/underlay.nix
    ../../common/settings.nix
    ../../common/firewall.nix
    ../../common/users.nix
    ./disk-config.nix
  ];

  networking.hostName = "ceridwen";
  networking.domain = "";
  networking.useDHCP = false;
  networking.interfaces.eno1.useDHCP = true;
  networking.interfaces.eno3.useDHCP = false;
  networking.interfaces.eno4.useDHCP = false;
  networking.bonds.bond0 = {
    interfaces = [ "eno3" "eno4" ];
    driverOptions = {
      mode = "802.3ad";
      lacp_rate = "fast";
    };
  };
  networking.interfaces.bond0 = {
    useDHCP = false;
    ipv4.addresses = [{
      address = "198.19.0.${toString config.melinoe.nodeId}";
      prefixLength = 24;
    }];
  };

  boot.loader.grub = {
    efiSupport = true;
    configurationLimit = 20;
    efiInstallAsRemovable = true;
    device = "nodev";
  };

  boot.loader.efi.efiSysMountPoint = "/boot/efi";
  boot.initrd.availableKernelModules = [ "sd_mod" "usbhid" "usb_storage" "mpt3sas" "ehci_pci" ];
  boot.initrd.kernelModules = [ "kvm-intel" ];

  melinoe.inetIfs = "eno1";
  melinoe.p2pIfs = "bond0";
  melinoe.nodeId = 6;
  melinoe.incusDefaultStorageSource = "/array/incus";
  melinoe.incusRootSize = "40GiB";
  melinoe.bgpPeers = [
    { id = 2; addr = "198.19.0.2"; }
    { id = 3; addr = "198.19.0.3"; }
    { id = 7; addr = "198.19.0.7"; }
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
