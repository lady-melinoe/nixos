{
  config,
  pkgs,
  inputs,
  melinoeNodeIntraIP,
  ...
}:
{
  system.stateVersion = "25.05";

  programs.nix-ld.enable = true;
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

  nix = {
    settings = {
      experimental-features = [
        "nix-command"
        "flakes"
      ];
      min-free = 2147483648;
      max-free = 5368709120;
      require-sigs = true;
    };
    gc = {
      automatic = true;
      dates = "daily";
      options = "--delete-older-than +5";
    };
  };

  boot = {
    kernelPackages = pkgs.linuxPackages_latest;
    tmp.cleanOnBoot = true;
    loader.grub.configurationLimit = 5;
    initrd = {
      availableKernelModules = [
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
      kernelModules = [
        "nvme"
        "kvm-intel"
      ];
    };
  };
  zramSwap.enable = true;
  systemd.oomd.enable = false;
  security = {
    audit = {
      enable = true;
      rules = [
        "-a always,exit -F arch=b64 -S execve -F euid=0 -F loginuid!=-1 -F key=root_cmd"
        "-a always,exit -F arch=b32 -S execve -F euid=0 -F loginuid!=-1 -F key=root_cmd"
      ];
    };
    auditd = {
      enable = true;
      settings = {
        log_format = "ENRICHED";
      };
    };
  };

  users.users.melinoe = {
    isNormalUser = true;
    home = "/home/melinoe";
    description = "melinoe_wuz_here";
    extraGroups = [
      "wheel"
      "networkmanager"
    ];
    shell = pkgs.bash;
    openssh.authorizedKeys.keys = [
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK4aBJqk5/c2gFqPTcK3II7733F5wGCdt1wEIjI2K9e5 melinoe@lilith"
    ];
  };

  melinoe.services.ssh = {
    enabled = true;
    fwOpenPorts = true;
    acceptEnv = [ "PROFILEUSER" ];
    userCA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINwS8hXHMP2ij1HmUL0N7oFU4+G8atQHtSRq9e8MOqkL SSH User CA";
    hostCert = "/etc/ssh/ssh_host_ed25519_key-cert.pub";
    hostCA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
    knownHosts = [
      {
        hosts = [
          "*.infra.melinoe.xyz"
          "*.intra.melinoe.xyz"
          "198.18.0.*"
          "198.19.0.*"
          "198.19.1.*"
          "198.19.3.*"
          "198.51.100.*"
        ];
        certAuthority = true;
        publicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T";
      }
    ];
  };

  services.qemuGuest.enable = true;
  systemd.services.qemu-guest-agent.unitConfig.ConditionVirtualization = "vm";

  services.chrony = {
    enable = true;
    servers = [ "time.uwa.edu.au:123" ];
  };

  services.iperf3.enable = true;
  services.glances.enable = true;
  services.glances.port = 61208;
  services.glances.extraArgs = [
    "--webserver"
    "-B"
    (melinoeNodeIntraIP config.melinoe.node.id)
  ];

  boot.kernel.sysctl = {
    "net.ipv6.conf.all.autoconf" = false;
    "net.ipv6.conf.all.accept_ra" = false;
    "net.ipv4.ip_forward" = true;
    "net.ipv4.conf.all.forwarding" = 1;
    "net.ipv4.conf.default.forwarding" = 1;
  };

  networking.domain = "infra.melinoe.xyz";
  networking.search = [
    "intra.melinoe.xyz"
    "ucc.asn.au"
  ];
  melinoe.services.melinoe-route.enabled = true;
  melinoe.services.haproxy = {
    backendNodes = [
      {
        name = "nginx1";
        address = "198.18.1.1";
      }
      {
        name = "nginx2";
        address = "198.18.1.2";
      }
    ];
    proxyProtocolAllowedSources = [
      "192.168.11.2"
    ];
    frontendPorts = {
      fe_http = 80;
      fe_https = 443;
      fe_http_prxbp = 6080;
      fe_https_prxbp = 6443;
      fe_proxy_http = 1080;
      fe_proxy_https = 1443;
    };
  };

  melinoe.node.networking = {
    wireguardBasePort = 64512;
    hostInternalPortAllNet.tcp = [ 5201 ]; # iperf3
  };
  melinoe.cluster.virtualMachines = [
    {
      iface = "vm-npm";
      ip = "198.18.1.1";
      specialHostAccess.tcp = [ 8002 ]; # eilidh drive on Benzaiten
    }
    {
      iface = "vm-npmalt";
      ip = "198.18.1.2";
      specialHostAccess.tcp = [ 8002 ]; # eilidh drive on Benzaiten
    }
    {
      iface = "vm-vaultwarden";
      ip = "198.18.1.3";
    }
    {
      iface = "vm-authentik";
      ip = "198.18.1.4";
    }
    {
      iface = "vm-homepage";
      ip = "198.18.1.5";
      tcp = [ 8080 ];
      udp = [ 8080 ];
      specialHostAccess.tcp = [ 61208 ]; # glances for monitoring
    }
    {
      iface = "vm-mailserver";
      ip = "198.18.1.6";
      tcp = [
        25
        465
      ];
    }
    {
      iface = "vm-radicale";
      ip = "198.18.1.7";
    }
    {
      iface = "vm-1710pack";
      ip = "198.18.1.8";
      tcp = [ 25565 ];
      udp = [ 25565 ];
    }
    {
      iface = "vm-website";
      ip = "198.18.1.9";
    }
    {
      iface = "vm-mmmanager";
      ip = "198.18.1.10";
    }
    {
      iface = "vm-drasl";
      ip = "198.18.1.11";
      tcp = [ 57843 ];
      udp = [ 57843 ];
    }
    {
      iface = "vm-calibre";
      ip = "198.18.1.12";
    }
    {
      iface = "vm-gitlab";
      ip = "198.18.1.13";
      outboundDropIP = [ "35.190.167.255" ];
    }
    {
      iface = "vm-snappymail";
      ip = "198.18.1.14";
    }
    {
      iface = "vm-dovecot";
      ip = "198.18.1.15";
      tcp = [ 993 ];
    }
    {
      iface = "vm-stash";
      ip = "198.18.1.16";
      outbound-via-node = 1;
    }
    {
      iface = "vm-ocis";
      ip = "198.18.1.17";
    }
    {
      iface = "vm-collabora";
      ip = "198.18.1.18";
    }
    {
      iface = "vm-ociscollab";
      ip = "198.18.1.19";
    }
    {
      iface = "vm-play";
      ip = "198.18.1.20";
      outbound-via-node = 1;
    }
    {
      iface = "vm-devicebridge";
      ip = "198.18.3.0/24";
      udp = [ 51820 ];
    }
  ];
}
