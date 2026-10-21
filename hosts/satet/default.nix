{ profiles, ... }:
{
  imports = [
    profiles.server-hardware
    profiles.bios-grub
    profiles.site-ucc
  ];

  melinoe = {
    disk = {
      device = "/dev/sde";
      boot = "bios";
      externalMounts = {
        "/array" = {
          label = "array";
          subvolume = "@array";
        };
        "/nix" = {
          label = "array";
          subvolume = "@nix";
          options = [ "noatime" ];
        };
      };
    };

    uplinks = [
      {
        interfaces = [ "enp2s0f0" ];
        address = "130.95.13.233/25";
        gateway = "130.95.13.129";
      }
    ];

    melnode.peers = {
      phaesyle = 1;
      ceridwen = 0;
      benzaiten = 0;
      arke = 0;
      lachesis = 2;
      atropos = 2;
      nephthys = 0;
    };
  };
}
