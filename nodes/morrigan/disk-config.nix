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
              size = "4G";
              content = {
                type = "filesystem";
                format = "vfat";
                mountpoint = "/boot/efi";
              };
            };
            root = {
              name = "nixroot";
              size = "900G";
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
                    swap.swapfile.size = "32G";
                  };
                  "/@array" = {
                    mountpoint = "/array";
                  };
                };
              };
            };
            oldroot = {
              name = "voidroot";
              size = "100%";
              content = {
                type = "btrfs";
                mountpoint = "/btrfs-voidroot";
                mountOptions = [
                  "defaults"
                  "subvolid=5"
                  "compress-force=zstd:2"
                  "ssd"
                  "discard=async"
                  "space_cache=v2"
                ];
                subvolumes = {
                  "/@root" = {
                  };
                  "/@home" = {
                  };
                  "/@old-stuff" = {
                  };
                  "/@swap" = {
                  };
                };
              };
            };
          };
        };
      };
    };
  };
}
