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
      device = "/dev/sda";
      externalMounts = {
        "/array" = {
          name = "array-2";
          label = "array-2";
        };
      };
    };

    uplinks = [
      {
        interfaces = [ "eno1" ];
        address = "130.95.13.235/25";
        gateway = "130.95.13.129";
      }
    ];

    melnode.peers = {
      phaesyle = 1;
      benzaiten = 0;
      arke = 0;
      lachesis = 2;
      atropos = 2;
      morrigan = 4;
      satet = 0;
      nephthys = 0;
    };
  };
}
