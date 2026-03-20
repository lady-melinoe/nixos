{ config, lib, pkgs, ... }:
let
  inherit (lib) mkOption types;
  nodeID = config.melinoe.nodeId;
  incusStorageSource = config.melinoe.incusDefaultStorageSource;
  incusRootSize = config.melinoe.incusRootSize;
  incusPreseedEnabled = config.melinoe.enableIncusPreseed;
  configureIface = pkgs.writeShellScriptBin "configure-iface" ''
    #!/usr/bin/env bash
    IFACE="$1"

    if [ -z "$IFACE" ] ; then
      echo "Usage: $0 <iface>"
      exit 1
    fi
    echo 1 > /proc/sys/net/ipv4/conf/$IFACE/forwarding
    echo 1 > /proc/sys/net/ipv4/conf/$IFACE/proxy_arp
    ip addr replace 198.18.0.${toString nodeID}/32 dev "$IFACE"
  '';
  disableBtrfsQuotas = pkgs.writeShellScript "disable-btrfs-quotas" ''
    #!/usr/bin/env bash
    for mnt in / /array; do
      if ! mountpoint -q "$mnt"; then
        continue
      fi

      fsType="$(findmnt -n -o FSTYPE --target "$mnt" || true)"
      if [ "$fsType" != "btrfs" ]; then
        continue
      fi

      btrfs quota disable "$mnt" || true
    done
  '';
in {
  options.melinoe.enableIncusPreseed = mkOption {
    type = types.bool;
    default = true;
    description = "Whether to apply the default Incus preseed (storage pool and profile).";
  };

  config = {
    virtualisation.incus.enable = true;
    users.users.melinoe.extraGroups = [ "incus-admin" ];
    virtualisation.incus.preseed = lib.mkIf incusPreseedEnabled {
      config = {
        "core.https_address" = "198.18.0.${toString nodeID}:8008";
      };
      networks = [];
      profiles = [
        {
          config = { };
          description = "Default Incus profile";
          devices = {
            root = {
              path = "/";
              pool = "default";
              type = "disk";
            };
          };
          name = "default";
        }
      ];
      storage_volumes = [ ];
      storage_pools = [
        {
          config = {
            source = incusStorageSource;
          };
          driver = "btrfs";
          name = "default";
        }
      ];
      projects = [
        {
          config = {
            "features.images" = "true";
            "features.networks" = "true";
            "features.networks.zones" = "true";
            "features.profiles" = "true";
            "features.storage.buckets" = "true";
            "features.storage.volumes" = "true";
          };
          description = "Default Incus project";
          name = "default";
        }
      ];
      certificates = [ ];
    };

    system.activationScripts.incusConfigureIface = {
      text = ''
        mkdir -p /etc/incus/hooks
        ln -sf ${configureIface}/bin/configure-iface /etc/incus/hooks/configure-iface
      '';
    };

    systemd.services.disable-btrfs-quotas = {
      description = "Disable btrfs quotas on main mounts";
      after = [ "local-fs.target" ];
      path = [ pkgs.util-linux pkgs.btrfs-progs ];
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${disableBtrfsQuotas}";
      };
    };

    systemd.timers.disable-btrfs-quotas = {
      description = "Run btrfs quota disabling every 10 minutes";
      wantedBy = [ "timers.target" ];
      timerConfig = {
        OnBootSec = "0";
        OnUnitActiveSec = "10min";
        Unit = "disable-btrfs-quotas.service";
        Persistent = true;
      };
    };
  };
}
