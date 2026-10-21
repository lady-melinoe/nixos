{
  inputs,
  pkgs,
  ...
}:
let
  kernelPkgs = inputs.nixpkgs-kernel.legacyPackages.${pkgs.stdenv.hostPlatform.system};
in
{
  nixpkgs.overlays = [
    (_: _: { inherit (kernelPkgs) aggregateModules makeModulesClosure; })
  ];

  boot = {
    kernelPackages = kernelPkgs.linuxPackages_latest;
    tmp.cleanOnBoot = true;
    loader.grub.configurationLimit = 5;
    kernel.sysctl = {
      "net.ipv6.conf.all.autoconf" = false;
      "net.ipv6.conf.all.accept_ra" = false;
      "net.ipv4.ip_forward" = true;
      "net.ipv4.conf.all.forwarding" = 1;
      "net.ipv4.conf.default.forwarding" = 1;
    };
  };

  zramSwap.enable = true;
  systemd.oomd.enable = false;
}
