{
  config,
  lib,
  pkgs,
  melinoeNodeIntraIP,
  ...
}:
let
  inherit (lib) mkOption mkIf types;
  cfg = config.melinoe;
  mCfg = cfg.services.melnode;

  melnodeKernelPackage =
    config.boot.kernelPackages.callPackage ./melinoe-vpn-kernelspace/package.nix
      { };
  netCfg = cfg.node.networking;
  nodeID = cfg.node.id;

  hostAddr = melinoeNodeIntraIP nodeID;

  tunPrefix = "node-";
  controlSocket = "/run/melnode/control.sock";
  dpSocket = "/run/melnode-dp/dp.sock";

  melnodeGoBinary = pkgs.buildGoModule {
    pname = "melnode";
    version = "0.0.2";

    src = ./melinoe-vpn-userspace/src;
    subPackages = [
      "cmd/melnode-dp"
      "cmd/melnode-cp"
    ];
    vendorHash = "sha256-qG9O8ed6bK0WpaQxof+pxsMa7IudmB0rnGTWYjuXm2g=";
  };

  dpBin = "${mCfg.package}/bin/melnode-dp";
  cpBin = "${mCfg.package}/bin/melnode-cp";

  peerIdStr = peer: toString peer.id;

  resolveEndpointHost =
    peer:
    if peer.endpoint != null then
      peer.endpoint
    else
      (cfg.nodePublicInfo.${peerIdStr peer} or { defaultEndpoint = null; }).defaultEndpoint;

  formatEndpoint =
    host:
    if lib.hasInfix ":" host then
      "[${host}]:${toString mCfg.port}"
    else
      "${host}:${toString mCfg.port}";

  isIpLiteral =
    host: lib.hasInfix ":" host || builtins.match "[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+" host != null;

  mkLink =
    peer:
    let
      host = resolveEndpointHost peer;
    in
    {
      peerid = peer.id;
      peerPubkey = cfg.nodePublicInfo.${peerIdStr peer}.wgPubkey;
      prependCount = peer.prependCount;
    }
    // lib.optionalAttrs (host != null) { endpoint = formatEndpoint host; };

  pubIps = lib.filter (ip: ip != null) (map (entry: entry.pub_ip or null) netCfg.uplinks);

  helper = pkgs.writers.writePython3Bin "melnode-helper" {
    doCheck = false;
  } (builtins.readFile ./melinoe-vpn-userspace/helper-scripts/melnode-helper.py);

  mnctl = pkgs.writers.writePython3Bin "mnctl" {
    doCheck = false;
  } (builtins.readFile ./melinoe-vpn-userspace/helper-scripts/mnctl.py);

  helperConfig = pkgs.writeText "melnode-helper.json" (
    builtins.toJSON {
      node_id = nodeID;
      table_base = netCfg.vmOutboundMarkBase;
      tun_prefix = tunPrefix;
      control_socket = controlSocket;
      pub_ips = pubIps;
      extra_routes = mCfg.extraRoutes;
      vm_ifaces = lib.optionals cfg.node.isVMHost (map (vm: vm.iface) cfg.cluster.virtualMachines);
    }
  );

  helperCmd = "${helper}/bin/melnode-helper --config ${helperConfig}";

  mkHook =
    kind:
    pkgs.writeShellScript "melnode-tun-${kind}-hook" ''
      exec ${helperCmd} hook-${kind} "$@"
    '';

  tomlFormat = pkgs.formats.toml { };

  cpConfig = tomlFormat.generate "melnode-cp.toml" (
    {
      localID = nodeID;
      localPort = mCfg.port;
      localPrivkeyPath = mCfg.privateKeyFile;
      mtu = mCfg.mtu;
      inherit tunPrefix;
      fwmark = netCfg.uplinkFwMark;
      identityPrefix = "${hostAddr}/32";
      controlSocket = controlSocket;
      introspectListen = ":${toString mCfg.introspectPort}";
      introspectIntervalMs = mCfg.introspectIntervalMs;
      tunCreateHookBin = "${mkHook "create"}";
      tunDestroyHookBin = "${mkHook "destroy"}";
      link = map mkLink netCfg.peers;
    }
    // (
      if mCfg.dataplane == "kernel" then { kernelDataplane = true; } else { dataplaneSocket = dpSocket; }
    )
  );

  toolPath = [
    pkgs.iproute2
    pkgs.nftables
  ];

  # Hot-swaps the melnode kernel module to the build in this generation, so a
  # `nixos-rebuild switch` picks up module changes without a reboot.
  #
  # modprobe can't do this: NixOS points it at the *booted* generation's module
  # tree, so a module built after boot is invisible to it. The new .ko is
  # loaded by store path with insmod instead (its dependencies still come from
  # the booted tree, which is fine because the kernel is the same).
  #
  # Runs as melnode-cp's ExecStartPre, i.e. after the old melnode-cp has
  # stopped. Unloading the module tears down the tuns and links; melnode-cp
  # recreates them on start, and melnode-helper's reconcile loop fixes up the
  # policy routing.
  kernelModuleSwap = pkgs.writeShellScript "melnode-kmod-swap" ''
    export PATH=${
      lib.makeBinPath [
        pkgs.kmod
        pkgs.coreutils
        pkgs.gnused
        pkgs.findutils
      ]
    }
    set -u

    marker=/run/melnode-kmod-loaded
    log() { echo "melnode-kmod: $*"; }

    ko=$(find ${mCfg.kernelDataplane.package}/lib/modules -name 'melnode.ko*' -print -quit)
    if [ -z "$ko" ]; then
      log "ERROR: no melnode.ko in ${mCfg.kernelDataplane.package}"
      exit 1
    fi

    load_deps() {
      for dep in $(modinfo -F softdep "$ko" | sed -E 's/(pre|post)://g') \
                 $(modinfo -F depends "$ko" | tr ',' ' '); do
        modprobe -q "$dep" || true
      done
    }

    if [ -d /sys/module/melnode ]; then
      want=$(modinfo -F srcversion "$ko")
      have=$(cat /sys/module/melnode/srcversion 2>/dev/null || true)
      if { [ -n "$want" ] && [ "$want" = "$have" ]; } || [ "$(cat "$marker" 2>/dev/null)" = "$ko" ]; then
        log "already running this build, nothing to swap"
        exit 0
      fi

      running=$(uname -r)
      built_for=$(modinfo -F vermagic "$ko" | cut -d' ' -f1)
      if [ "$running" != "$built_for" ]; then
        log "WARNING: new module is built for kernel $built_for but $running is running; keeping the loaded module until reboot"
        exit 0
      fi

      log "swapping module: $have -> $want"
      tries=0
      until rmmod melnode 2>/dev/null; do
        tries=$((tries + 1))
        if [ "$tries" -ge 10 ]; then
          log "WARNING: could not unload melnode ($(rmmod melnode 2>&1)); keeping the loaded module"
          exit 0
        fi
        sleep 0.5
      done
    fi

    load_deps
    if insmod "$ko"; then
      echo "$ko" > "$marker"
      log "loaded $ko"
      exit 0
    fi

    log "ERROR: insmod of $ko failed; falling back to the booted generation's module"
    rm -f "$marker"
    if modprobe melnode; then
      log "restored the booted generation's module"
      exit 0
    fi
    exit 1
  '';
in
{
  options.melinoe.services.melnode = {
    enabled = mkOption {
      type = types.bool;
      default = false;
      description = "Enable the melnode mesh VPN (replaces WireGuard + BGP + IPIP): the melnode-dp data plane and the melnode-cp control plane.";
    };

    port = mkOption {
      type = types.port;
      default = 60198;
      description = ''
        UDP port every node listens on, and every link dials. All nodes use
        the same port; no per-peer ports.
      '';
    };

    introspectPort = mkOption {
      type = types.port;
      default = 60198;
      description = ''
        TCP port for melnode's read-only introspection API (/summary, /links,
        /routes, /prefixes, /tuns; never the write endpoints). Reachable only
        from the host range (melinoe.node.networking.specialHostAccess), so any
        node can view any other node's state. Served from a snapshot rebuilt
        every introspectIntervalMs, never the live state.
      '';
    };

    introspectIntervalMs = mkOption {
      type = types.ints.between 100 60000;
      default = 1000;
      description = ''
        How often, in milliseconds, the TCP introspection API's snapshot is
        rebuilt: the most out of date its answers can be (each response
        carries its age), and how often it reads the data plane whether or
        not anyone is asking. The control socket's views stay live.
      '';
    };

    mtu = mkOption {
      type = types.ints.between 576 65000;
      default = 1416;
      description = ''
        MTU of every node-<id> tun. 1416 is WireGuard's 1420 (1500 minus IPv6,
        UDP and WireGuard framing) minus melnode's own 4-byte routing header,
        so a full-size packet still fits a 1500-byte underlay.
      '';
    };

    privateKeyFile = mkOption {
      type = types.str;
      default = "/etc/melinoe/wg.privatekey";
      description = "Path to this node's base64 Curve25519 private key (same format as a WireGuard key).";
    };

    extraRoutes = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "Additional IPv4 prefixes or host routes this node advertises into the mesh.";
    };

    package = mkOption {
      type = types.package;
      default = melnodeGoBinary;
      readOnly = true;
      description = "The built melnode package (bin/melnode-dp and bin/melnode-cp).";
    };

    dataplane = mkOption {
      type = types.enum [
        "userspace"
        "kernel"
      ];
      default = "userspace";
      description = ''
        Which data plane melnode-cp attaches to. "userspace" (default) runs
        melnode-dp as its own systemd service, as before. "kernel" attaches
        to the melnode kernel module (modules/melinoe-vpn-kernelspace) over generic
        netlink instead - no melnode-dp process at all - which requires
        melinoe.services.melnode.kernelDataplane.enable to be set too (that
        loads the module; this makes melnode-cp actually use it).

        The kernel module is new and not yet as battle-tested as melnode-dp;
        treat "kernel" as experimental.
      '';
    };

    kernelDataplane = {
      enable = mkOption {
        type = types.bool;
        default = false;
        description = ''
          Load the melnode kernel module (modules/melinoe-vpn-kernelspace)
          instead of running melnode-dp in userspace. GPLv2, built out-of-tree
          against `config.boot.kernelPackages` for this host.

          Implements the full "melnode" generic netlink (genl) family
          (device/link/route/tun lifecycle, Noise handshake, forwarding).
          Each netlink socket that configures the family gets its own
          independent instance, which is destroyed when the socket closes,
          so restarting melnode-cp restarts the data plane. Still early:
          enable on nodes you can watch closely and roll back easily, not as
          a default.

          With dataplane = "kernel", `nixos-rebuild switch` hot-swaps the
          module: melnode-cp restarts, unloads the running melnode.ko, and
          loads the new build, so module changes don't need a reboot. Tunnels
          are down for a few seconds while it happens. If the generation
          changes the kernel itself, the loaded module is kept until reboot.
        '';
      };

      package = mkOption {
        type = types.package;
        default = melnodeKernelPackage;
        readOnly = true;
        description = "The built melnode.ko kernel module package.";
      };
    };
  };

  config = {
    boot = mkIf mCfg.kernelDataplane.enable {
      extraModulePackages = [ mCfg.kernelDataplane.package ];
      kernelModules = [ "melnode" ];
    };

    assertions = [
      {
        assertion = mCfg.enabled || mCfg.extraRoutes == [ ];
        message = "melinoe.services.melnode.extraRoutes requires melinoe.services.melnode.enabled to be true.";
      }
      {
        assertion = !mCfg.enabled || netCfg.enabled;
        message = "melinoe.node.networking.enabled must be true when melinoe.services.melnode.enabled is true (melinoe.node.networking.uplinks is an uplink property and pub_ips are derived from it).";
      }
      {
        assertion =
          !mCfg.kernelDataplane.enable
          ||
            builtins.readFile ./melinoe-vpn-userspace/src/dpproto/melnode_genl.h
            == builtins.readFile ./melinoe-vpn-kernelspace/src/melnode_genl.h;
        message = "modules/melinoe-vpn-kernelspace/src/melnode_genl.h and modules/melinoe-vpn-userspace/src/dpproto/melnode_genl.h are incompatible.";
      }
      {
        assertion = !mCfg.enabled || mCfg.dataplane != "kernel" || mCfg.kernelDataplane.enable;
        message = "melinoe.services.melnode.dataplane = \"kernel\" requires melinoe.services.melnode.kernelDataplane.enable = true (it loads the module; this makes melnode-cp use it).";
      }
    ]
    ++ lib.optionals mCfg.enabled (
      lib.concatMap (
        peer:
        let
          id = peerIdStr peer;
          info = cfg.nodePublicInfo.${id} or null;
          host = if info == null then null else resolveEndpointHost peer;
        in
        [
          {
            assertion = peer.id != nodeID;
            message = "melinoe.node.networking.peers: node ${id} lists itself as a peer.";
          }
          {
            assertion = info != null;
            message = "melinoe.node.networking.peers: peer ${id} has no melinoe.nodePublicInfo entry (its public key is needed).";
          }
          {
            assertion = host == null || isIpLiteral host;
            message = "melinoe.node.networking.peers: the endpoint for peer ${id} (${toString host}) must be an IP literal - melnode does not resolve hostnames.";
          }
        ]
      ) netCfg.peers
    );

    melinoe.node.networking.openPorts.udp = lib.mkIf mCfg.enabled [ mCfg.port ];
    melinoe.node.networking.specialHostAccess.tcp = lib.mkIf mCfg.enabled [ mCfg.introspectPort ];

    environment.systemPackages = lib.mkIf mCfg.enabled [ mnctl ];

    systemd.services.melnode-dp = lib.mkIf (mCfg.enabled && mCfg.dataplane == "userspace") {
      description = "melnode data plane (Noise tunnels, tuns, forwarding)";
      before = [ "network-online.target" ];
      wantedBy = [ "multi-user.target" ];
      after = [
        "network.target"
        "melinoe-inet-setup.service"
      ];
      wants = [
        "network.target"
        "melinoe-inet-setup.service"
      ];
      stopIfChanged = false;
      serviceConfig = {
        ExecStartPre = "-+${pkgs.kmod}/bin/rmmod melnode";
        ExecStart = "${dpBin} -socket ${dpSocket}";
        Restart = "always";
        RestartSec = 1;
        RuntimeDirectory = "melnode-dp";
        AmbientCapabilities = [ "CAP_NET_ADMIN" ];
        CapabilityBoundingSet = [ "CAP_NET_ADMIN" ];
      };
    };

    systemd.services.melnode-cp = lib.mkIf mCfg.enabled {
      description = "melnode control plane (liveness, path-vector routing, host integration)";
      before = [ "network-online.target" ];
      wantedBy = [ "multi-user.target" ];
      after = [
        "network.target"
        "melinoe-inet-setup.service"
        "nftables.service"
      ]
      ++ lib.optional (mCfg.dataplane == "userspace") "melnode-dp.service";
      wants = [
        "network.target"
        "melinoe-inet-setup.service"
        "nftables.service"
      ];
      requires = lib.optional (mCfg.dataplane == "userspace") "melnode-dp.service";
      stopIfChanged = false;
      restartTriggers = [ (builtins.hashString "sha256" config.networking.nftables.ruleset) ];
      path = toolPath;
      serviceConfig = {
        ExecStartPre = lib.optional (mCfg.dataplane == "kernel") "+${kernelModuleSwap}";
        ExecStart = "${cpBin} -config ${cpConfig}";
        Restart = "always";
        RestartSec = 1;
        RuntimeDirectory = "melnode";
        AmbientCapabilities = [ "CAP_NET_ADMIN" ];
        CapabilityBoundingSet = [ "CAP_NET_ADMIN" ];
      };
    };

    systemd.services.melnode-helper = lib.mkIf mCfg.enabled {
      description = "melnode helper (route advertisement, per-tun policy routing)";
      before = [ "network-online.target" ];
      wantedBy = [ "multi-user.target" ];
      after = [
        "melnode-cp.service"
        "nftables.service"
      ];
      wants = [ "melnode-cp.service" ];
      stopIfChanged = false;
      path = toolPath;
      serviceConfig = {
        ExecStart = "${helperCmd} daemon";
        Restart = "always";
        RestartSec = 1;
        AmbientCapabilities = [ "CAP_NET_ADMIN" ];
        CapabilityBoundingSet = [ "CAP_NET_ADMIN" ];
      };
    };
  };
}
