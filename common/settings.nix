{ config, pkgs, ... }:

{
  boot.tmp.cleanOnBoot = true;
  zramSwap.enable = true;
  systemd.oomd.enable = false;
  boot.initrd.availableKernelModules = [ "ata_piix" "uhci_hcd" "xen_blkfront" "vmw_pvscsi" "sd_mod" "usbhid" "usb_storage" "mpt3sas" "ehci_pci" ];
  boot.initrd.kernelModules = [ "nvme" "kvm-intel" ];
  services.openssh.enable = true;
  services.qemuGuest.enable = true;
  systemd.services.qemu-guest-agent.unitConfig.ConditionVirtualization = "vm";
  services.openssh.ports = [ 22 ];
  services.openssh.extraConfig = ''
    HostCertificate /etc/ssh/ssh_host_ed25519_key-cert.pub
    TrustedUserCAKeys /etc/ssh/ssh-user-ca.pub
  '';
  environment.etc."ssh/ssh_known_hosts".text = ''
    @cert-authority *.infra.melinoe.xyz,198.18.0.*,198.19.0.*,198.19.1.*,198.51.100.* ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T
  '';
  environment.etc."ssh/ssh-user-ca.pub".text = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINwS8hXHMP2ij1HmUL0N7oFU4+G8atQHtSRq9e8MOqkL SSH User CA";
  environment.etc."ssh/ssh-user-ca.pub".mode = "0644";
  environment.etc."ssh/ssh-host-ca.pub".text = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
  environment.etc."ssh/ssh-host-ca.pub".mode = "0644";
  environment.etc."ssh/ssh_host_ed25519_key.pub".text = config.melinoe.sshHostKeyPub;
  environment.etc."ssh/ssh_host_ed25519_key.pub".mode = "0644";
  environment.etc."ssh/ssh_host_ed25519_key-cert.pub".text = config.melinoe.sshHostCertPub;
  environment.etc."ssh/ssh_host_ed25519_key-cert.pub".mode = "0644";
  nix.settings.experimental-features = [ "nix-command" "flakes" ];
  nix.settings.require-sigs = false;
  system.stateVersion = "25.05";
  environment.systemPackages = [ pkgs.git pkgs.tcpdump pkgs.nftables pkgs.jq pkgs.screen pkgs.btop pkgs.iperf3 pkgs.iptables pkgs.python3 ];
  boot.kernelPackages = pkgs.linuxPackages_latest;
  boot.kernel.sysctl = {
    "net.ipv6.conf.all.autoconf" = false;
    "net.ipv6.conf.all.accept_ra" = false;
    "net.ipv4.ip_forward" = true;
  };
}
