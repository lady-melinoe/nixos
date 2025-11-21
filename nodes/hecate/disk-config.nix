{
  disko.devices = {
    disk = {
      main = {
        type = "disk";
        device = "/dev/nvme0n1";
        content = {
          type = "gpt";
          partitions = {
            EFI = {
              name = "EFI";
              type = "EF00";
              size = "256M";
              content = {
                type = "filesystem";
                format = "vfat";
                mountpoint = "/boot/efi";
              };
            };
            root = {
              name = "BTRFS";
              size = "100%";
              content = {
                type = "btrfs";
                mountpoint = "/btrfs";
                mountOptions = [
                  "defaults"
                  "subvolid=5"
                ];
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
                    swap.swapfile.size = "2G";
                  };
                  "/@incus" = {};
                };
              };
            };
          };
        };
      };
    };
    nodev = {
      big = {
        device = "/dev/disk/by-label/big";
        fsType = "btrfs";
        mountpoint = "/big";
      };
      uccdrives = {
        device = "/dev/disk/by-label/uccdrives";
        fsType = "btrfs";
        mountpoint = "/array";
      };
    };
  };
}
