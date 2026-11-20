{ profiles, ... }:
{
  imports = [
    profiles.hw-membervm
    profiles.efi-grub
  ];

  melinoe = {
    incus.enable = false;
    haproxy.enable = false;

    build.remote = [
      "nephthys"
      "ceridwen"
      "benzaiten"
      "satet"
    ];

    disk.device = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-scsi0";

    uplinks = [
      {
        interfaces = [ "ens18" ];
        dhcp = true;
      }
    ];

    melnode.peers = {
      arke = 0;
    };
  };
}
