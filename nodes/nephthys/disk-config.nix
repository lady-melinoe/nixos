{
  disko.devices = {
    disk = {
      main = {
        type = "disk";
        device = "/dev/sda";
        content = {
          type = "gpt";
          partitions = {
            GRUB = {
              name = "GRUB";
              type = "EF02";
              size = "2M";
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
                  "/@nix" = { };
                  "/@swap" = {
                    mountpoint = "/.swap";
                    swap.swapfile.size = "2G";
                  };
                  "/@array" = { };
                };
              };
            };
          };
        };
      };
    };
    nodev = {
      "array" = {
        device = "/dev/disk/by-label/array";
        fsType = "btrfs";
        mountpoint = "/array";
        mountOptions = [ "subvol=@array" ];
      };
      "nix" = {
        device = "/dev/disk/by-label/array";
        fsType = "btrfs";
        mountpoint = "/nix";
        mountOptions = [
          "subvol=@nix"
          "noatime"
        ];
      };
    };
  };
}
