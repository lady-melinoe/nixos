{ config, lib, ... }:
let
  inherit (lib) mkOption types;
  cfg = config.melinoe.disk;

  efi = {
    name = "EFI";
    type = "EF00";
    size = cfg.efiSize;
    content = {
      type = "filesystem";
      format = "vfat";
      mountpoint = "/boot/efi";
    };
  };

  biosBoot = {
    name = "GRUB";
    type = "EF02";
    size = "2M";
  };

  subvolumes = {
    "/@root" = {
      mountpoint = "/";
      mountOptions = [
        "compress-force=zstd:2"
        "ssd"
        "discard=async"
        "space_cache=v2"
      ];
    };
    "/@home" = {
      mountpoint = "/home";
      mountOptions = [ "relatime" ];
    };
    "/@nix" = {
      mountpoint = "/nix";
      mountOptions = [ "noatime" ];
    };
    "/@swap" = {
      mountpoint = "/.swap";
      swap.swapfile.size = cfg.swapSize;
    };
    "/@array".mountpoint = "/array";
  };

  isExternal = subvolume: subvolume ? mountpoint && cfg.externalMounts ? ${subvolume.mountpoint};
in
{
  options.melinoe.disk = {
    device = mkOption { type = types.str; };
    boot = mkOption {
      type = types.enum [
        "efi"
        "bios"
      ];
      default = "efi";
    };
    efiSize = mkOption {
      type = types.str;
      default = "256M";
    };
    rootName = mkOption {
      type = types.str;
      default = "BTRFS";
    };
    rootSize = mkOption {
      type = types.str;
      default = "100%";
    };
    swapSize = mkOption {
      type = types.str;
      default = "2G";
    };
    extraPartitions = mkOption {
      type = types.attrsOf types.attrs;
      default = { };
    };
    externalMounts = mkOption {
      default = { };
      description = ''
        Filesystems on other disks, by mountpoint, mounted in place of the
        local subvolume of the same mountpoint, which is left in the layout empty.
      '';
      type = types.attrsOf (
        types.submodule (
          { name, config, ... }:
          {
            options = {
              name = mkOption {
                type = types.str;
                default = baseNameOf name;
              };
              label = mkOption { type = types.str; };
              subvolume = mkOption {
                type = types.nullOr types.str;
                default = null;
              };
              options = mkOption {
                type = types.listOf types.str;
                default = [ ];
              };
              mountOptions = mkOption {
                type = types.listOf types.str;
                readOnly = true;
                default = lib.optional (config.subvolume != null) "subvol=${config.subvolume}" ++ config.options;
              };
            };
          }
        )
      );
    };
  };

  config.disko.devices = {
    disk.main = {
      type = "disk";
      inherit (cfg) device;
      content = {
        type = "gpt";
        partitions =
          (if cfg.boot == "efi" then { EFI = efi; } else { GRUB = biosBoot; })
          // {
            root = {
              name = cfg.rootName;
              size = cfg.rootSize;
              content = {
                type = "btrfs";
                mountpoint = "/btrfs";
                mountOptions = [
                  "defaults"
                  "subvolid=5"
                ];
                subvolumes = lib.mapAttrs (
                  _: subvolume: if isExternal subvolume then { } else subvolume
                ) subvolumes;
              };
            };
          }
          // cfg.extraPartitions;
      };
    };
  }
  // lib.optionalAttrs (cfg.externalMounts != { }) {
    nodev = lib.mapAttrs' (
      mountpoint: mount:
      lib.nameValuePair mount.name (
        {
          device = "/dev/disk/by-label/${mount.label}";
          fsType = "btrfs";
          inherit mountpoint;
        }
        // lib.optionalAttrs (mount.mountOptions != [ ]) { inherit (mount) mountOptions; }
      )
    ) cfg.externalMounts;
  };
}
