{
  config,
  pkgs,
  inputs,
  ...
}:

{
  boot.tmp.cleanOnBoot = true;
  zramSwap.enable = true;
  systemd.oomd.enable = false;
  boot.initrd.availableKernelModules = [
    "ata_piix"
    "uhci_hcd"
    "xen_blkfront"
    "vmw_pvscsi"
    "sd_mod"
    "usbhid"
    "usb_storage"
    "mpt3sas"
    "ehci_pci"
  ];
  boot.initrd.kernelModules = [
    "nvme"
    "kvm-intel"
  ];

  services.chrony = {
    enable = true;
    servers = [ "time.uwa.edu.au:123" ];
  };

  services.openssh.enable = true;
  services.openssh.settings.PasswordAuthentication = false;
  services.openssh.settings.KbdInteractiveAuthentication = false;
  services.qemuGuest.enable = true;
  systemd.services.qemu-guest-agent.unitConfig.ConditionVirtualization = "vm";
  services.openssh.ports = [ 22 ];
  services.openssh.extraConfig = ''
    TrustedUserCAKeys /etc/ssh/ssh-user-ca.pub
    HostCertificate /etc/ssh/ssh_host_ed25519_key-cert.pub
  '';
  environment.etc."ssh/ssh_known_hosts".text = ''
    @cert-authority *.infra.melinoe.xyz,*.intra.melinoe.xyz,198.18.0.*,198.19.0.*,198.19.1.*,198.19.3.*,198.51.100.* ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T
  '';
  environment.etc."ssh/ssh-user-ca.pub".text =
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINwS8hXHMP2ij1HmUL0N7oFU4+G8atQHtSRq9e8MOqkL SSH User CA";
  environment.etc."ssh/ssh-user-ca.pub".mode = "0644";
  environment.etc."ssh/ssh-host-ca.pub".text =
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
  environment.etc."ssh/ssh-host-ca.pub".mode = "0644";
  nix.settings.experimental-features = [
    "nix-command"
    "flakes"
  ];
  nix.settings.require-sigs = true;
  programs.nix-ld.enable = true;
  system.stateVersion = "25.05";
  environment.systemPackages = [
    pkgs.git
    pkgs.tcpdump
    pkgs.nftables
    pkgs.jq
    pkgs.screen
    pkgs.btop
    pkgs.iperf3
    pkgs.iptables
    pkgs.python3
    pkgs.sl
    pkgs.compsize
    pkgs.aria2
    pkgs.nfs-utils
    pkgs.sshfs
    inputs.self.packages.${pkgs.stdenv.hostPlatform.system}.borg-beta
  ];
  boot.kernelPackages = pkgs.linuxPackages_latest;
  boot.kernel.sysctl = {
    "net.ipv6.conf.all.autoconf" = false;
    "net.ipv6.conf.all.accept_ra" = false;
    "net.ipv4.ip_forward" = true;
    "net.ipv4.conf.all.forwarding" = 1;
    "net.ipv4.conf.default.forwarding" = 1;
  };
  virtualisation.incus.enable = true;
  virtualisation.incus.package = pkgs.incus;
  systemd.services.incus-startup = {
    stopIfChanged = false;
    restartIfChanged = false;
    environment.TRIGGER = "test-1";
  };
  services.iperf3.enable = true;

  services.glances.enable = true;
  services.glances.port = 61208;
  services.glances.extraArgs = [
    "--webserver"
    "-B"
    "198.18.0.${toString config.melinoe.nodeId}"
  ];

  # HAProxy Configuration
  services.haproxy = {
    enable = true;
    config = ''
      global
          log /dev/log local0
          log /dev/log local1 notice
          daemon
          maxconn 50000

      defaults
          log     global
          mode    tcp
          option  tcplog
          option  redispatch
          timeout connect 5s
          timeout client  1m
          timeout server  1m
          default-server inter 3s fall 3 rise 2

      frontend fe_http
          bind *:80
          default_backend be_http

      frontend fe_https
          bind *:443
          tcp-request inspect-delay 5s
          tcp-request content accept if { req_ssl_hello_type 1 }
          default_backend be_https

      frontend fe_proxy_http
          bind *:1080 accept-proxy
          tcp-request connection reject if !{ src 192.168.11.2 }
          default_backend be_http

      frontend fe_proxy_https
          bind *:1443 accept-proxy
          tcp-request connection reject if !{ src 192.168.11.2 }
          tcp-request inspect-delay 5s
          tcp-request content accept if { req_ssl_hello_type 1 }
          default_backend be_https

      backend be_http
          balance source
          hash-type consistent
          option tcp-check
          server nginx1 198.18.1.1:1080 send-proxy check check-send-proxy
          server nginx2 198.18.1.2:1080 send-proxy check check-send-proxy

      backend be_https
          balance source
          hash-type consistent
          option tcp-check
          server nginx1 198.18.1.1:1443 send-proxy check check-send-proxy
          server nginx2 198.18.1.2:1443 send-proxy check check-send-proxy
    '';
  };

  users.users.melinoe = {
    isNormalUser = true;
    home = "/home/melinoe";
    description = "melinoe_wuz_here";
    extraGroups = [
      "wheel"
      "networkmanager"
      "incus-admin"
    ];
    shell = pkgs.bash;
  };
}
