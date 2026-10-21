{ modulesPath, profiles, ... }:
{
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    profiles.server-hardware
    profiles.efi-grub
    profiles.site-ucc
  ];

  melinoe = {
    build.remote = [
      "nephthys"
      "ceridwen"
      "benzaiten"
    ];

    disk.device = "/dev/sda";

    uplinks = [
      {
        interfaces = [ "ens18" ];
        address = "130.95.13.236/25";
        gateway = "130.95.13.129";
      }
    ];

    melnode.peers = {
      phaesyle = 1;
      ceridwen = 0;
      benzaiten = 0;
      lachesis = 2;
      atropos = 2;
      morrigan = 4;
      satet = 0;
      nephthys = 0;
    };
  };
}
