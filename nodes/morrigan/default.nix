{ inputs, ... }:
{
  imports = [
    inputs.lanzaboote.nixosModules.lanzaboote
    ./desktop.nix
    ./fingerprint.nix
    ./hardware.nix
    ./overlays.nix
    ./plymouth.nix
  ];

  melinoe = {
    home = [ ../../home/melinoe ];

    disk = {
      device = "/dev/disk/by-id/nvme-CT2000P3PSSD8_2345E88426AC";
      efiSize = "4G";
      rootName = "nixroot";
      rootSize = "900G";
      swapSize = "32G";
      extraPartitions.oldroot = {
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
            "/@root" = { };
            "/@home" = { };
            "/@old-stuff" = { };
            "/@swap" = { };
          };
        };
      };
    };

    incus.enable = false;
    update.remote = false;
    haproxy.enable = false;
    build.remote = [
      "nephthys"
      "satet"
      "ceridwen"
      "benzaiten"
    ];

    uplinks = [
      {
        interfaces = [ "wlan0" ];
        dhcp = true;
        wifi = true;
      }
    ];

    melnode.peers = {
      phaesyle = 4;
      ceridwen = 4;
      arke = 4;
      atropos = 4;
    };
  };

  networking.hosts."0.0.0.0" = [
    "paradise-s1.battleye.com"
    "test-s1.battleye.com"
    "paradiseenhanced-s1.battleye.com"
  ];
}
