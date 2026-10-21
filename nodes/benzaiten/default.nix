{ profiles, ... }:
{
  imports = [
    profiles.server-hardware
    profiles.efi-grub
    profiles.serial-getty
    profiles.site-ucc
  ];

  melinoe = {
    disk = {
      device = "/dev/disk/by-id/wwn-0x5002538d40b108e2";
      externalMounts = {
        "/array" = {
          name = "array-1";
          label = "array-1";
        };
      };
    };

    uplinks = [
      {
        interfaces = [ "eno1" ];
        address = "130.95.13.234/25";
        gateway = "130.95.13.129";
      }
    ];

    melnode.peers = {
      phaesyle = 1;
      ceridwen = 0;
      arke = 0;
      lachesis = 2;
      atropos = 2;
      satet = 0;
      nephthys = 0;
    };
  };
}
