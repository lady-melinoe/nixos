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
  security.audit.enable = true;
  security.auditd.enable = true;
  services.qemuGuest.enable = true;
  security.auditd.settings = {
    log_format = "ENRICHED";
  };
  security.audit.rules = [
    "-a always,exit -F arch=b64 -S execve -F euid=0 -F loginuid!=-1 -F key=root_cmd"
    "-a always,exit -F arch=b32 -S execve -F euid=0 -F loginuid!=-1 -F key=root_cmd"
  ];
  systemd.services.qemu-guest-agent.unitConfig.ConditionVirtualization = "vm";
  services.openssh.ports = [ 22 ];
  services.openssh.enable = true;
  services.openssh.settings.PasswordAuthentication = false;
  services.openssh.settings.KbdInteractiveAuthentication = false;
  services.openssh.extraConfig = ''
    AcceptEnv PROFILEUSER
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
  nix.settings.min-free = 2147483648;
  nix.settings.max-free = 5368709120;
  nix.gc = {
    automatic = true;
    dates = "daily";
    options = "--delete-older-than +5";
  };
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
  virtualisation.incus.softDaemonRestart = true;
  services.iperf3.enable = true;
  services.glances.enable = true;
  services.glances.port = 61208;
  services.glances.extraArgs = [
    "--webserver"
    "-B"
    "198.18.0.${toString config.melinoe.nodeId}"
  ];

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
    openssh.authorizedKeys.keys = [
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK4aBJqk5/c2gFqPTcK3II7733F5wGCdt1wEIjI2K9e5 melinoe@lilith"
    ];
  };
  users.users.gitlab-deploy = {
    isSystemUser = true;
    group = "gitlab-deploy";
    home = "/var/empty";
    createHome = false;
    shell = pkgs.bash;
    hashedPassword = "!";
  };
  users.groups.gitlab-deploy = { };
  security.sudo.extraRules = [
    {
      users = [ "gitlab-deploy" ];
      commands = [
        {
          command = "/run/current-system/sw/bin/update-safe";
          options = [ "NOPASSWD" ];
        }
      ];
    }
  ];
  security.sudo.extraConfig = ''
    Defaults:gitlab-deploy env_reset
    Defaults:gitlab-deploy env_delete+="SSH_AUTH_SOCK SSH_CLIENT SSH_CONNECTION SSH_ORIGINAL_COMMAND SSH_TTY"
    Defaults:gitlab-deploy secure_path="/run/current-system/sw/bin"
  '';
}
