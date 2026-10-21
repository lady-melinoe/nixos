{ profiles, ... }:
{
  imports = [
    profiles.hw-oracle-vm
    profiles.efi-grub
    profiles.serial-grub
  ];

  melinoe = {
    build.remote = [
      "nephthys"
      "ceridwen"
      "benzaiten"
      "satet"
    ];

    disk.device = "/dev/disk/by-id/scsi-360536a83aec843a3a7a0f98fa1a70cce";

    uplinks = [
      {
        interfaces = [ "ens3" ];
        address = "198.19.1.4/24";
        gateway = "198.19.1.1";
      }
    ];

    melnode.peers = {
      phaesyle = 2;
      ceridwen = 2;
      benzaiten = 2;
      arke = 2;
      atropos = 0;
      satet = 2;
      nephthys = 2;
    };
  };
}
