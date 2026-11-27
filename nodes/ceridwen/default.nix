{ profiles, ... }:
{
  imports = [
    profiles.hw-ibm-x3550-m4
    profiles.efi-grub
    profiles.serial-getty
    profiles.site-ucc
  ];

  melinoe = {
    disk = {
      device = "/dev/disk/by-id/wwn-0x500a075113b96dc6";
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
