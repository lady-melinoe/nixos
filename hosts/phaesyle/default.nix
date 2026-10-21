{ modulesPath, profiles, ... }:
{
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    profiles.server-hardware
    profiles.efi-grub
  ];

  melinoe = {
    build.remote = [
      "nephthys"
      "ceridwen"
      "benzaiten"
      "satet"
    ];

    disk.device = "/dev/vda";

    uplinks = [
      {
        interfaces = [ "ens3" ];
        address = "103.249.239.233/24";
        gateway = "103.249.239.1";
      }
    ];

    melnode.peers = {
      ceridwen = 1;
      benzaiten = 1;
      arke = 1;
      lachesis = 2;
      atropos = 2;
      morrigan = 4;
      satet = 1;
      nephthys = 1;
    };
  };
}
