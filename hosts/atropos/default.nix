{ modulesPath, profiles, ... }:
{
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    profiles.server-hardware
    profiles.efi-grub
    profiles.serial-grub
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
        interfaces = [ "ens3" ];
        address = "198.19.1.5/24";
        gateway = "198.19.1.1";
      }
    ];

    melnode.peers = {
      phaesyle = 2;
      ceridwen = 2;
      benzaiten = 2;
      arke = 2;
      lachesis = 0;
      morrigan = 4;
      satet = 2;
      nephthys = 2;
    };
  };
}
