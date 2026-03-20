{ config, pkgs, ... }:

let
  nodeCertDir = ../nodes/${config.networking.hostName};
in

{
  boot.tmp.cleanOnBoot = true;
  zramSwap.enable = true;
  services.openssh.enable = true;
  services.openssh.ports = [ 22 ];
  services.openssh.extraConfig = ''
    HostCertificate /etc/ssh/ssh_host_ed25519_key-cert.pub
  '';
  environment.etc."ssh/ssh_host_ed25519_key.pub" = {
    source = "${nodeCertDir}/ssh_host_ed25519_key.pub";
    mode = "0644";
  };
  environment.etc."ssh/ssh_host_ed25519_key-cert.pub" = {
    source = "${nodeCertDir}/ssh_host_ed25519_key-cert.pub";
    mode = "0644";
  };
  nix.settings.experimental-features = [ "nix-command" "flakes" ];
  nix.settings.require-sigs = false;
  system.stateVersion = "25.05";
  environment.systemPackages = [ pkgs.git pkgs.tcpdump pkgs.nftables pkgs.jq pkgs.screen pkgs.btop pkgs.iperf3 pkgs.iptables ];
  boot.kernelPackages = pkgs.linuxPackages_latest;
  boot.kernel.sysctl = {
    "net.ipv6.conf.all.autoconf" = false;
    "net.ipv6.conf.all.accept_ra" = false;
    "net.ipv4.ip_forward" = true;
  };
}
