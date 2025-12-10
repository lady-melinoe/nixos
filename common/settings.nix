{ pkgs, ... }:

{
  boot.tmp.cleanOnBoot = true;
  zramSwap.enable = true;
  services.openssh.enable = true;
  services.openssh.ports = [ 22 ];
  nix.settings.experimental-features = [ "nix-command" "flakes" ];
  nix.settings.require-sigs = false;
  system.stateVersion = "25.05";
  environment.systemPackages = [ pkgs.git pkgs.tcpdump pkgs.nftables pkgs.jq pkgs.screen ];
  boot.kernelPackages = pkgs.linuxPackages_latest;
  boot.kernel.sysctl = {
    "net.ipv6.conf.all.autoconf" = false;
    "net.ipv6.conf.all.accept_ra" = false;
    "net.ipv4.ip_forward" = true;
  };
}
