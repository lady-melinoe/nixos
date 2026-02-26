{
  disko.devices = {
    disk = {
      main = {
        type = "disk";
        device = "/dev/sda";
        content = {
          type = "gpt";
          partitions = {
            MBR = {
              type = "EF02"; # for grub MBR
              size = "1M";
              priority = 1; # Needs to be first partition
              attributes = [ 0 ]; # partition attribute
            };
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
                  "/@array" = {
                    mountpoint = "/array";
                    mountOptions = [ "noatime" ];
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
