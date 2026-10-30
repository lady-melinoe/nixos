{
  config,
  lib,
  pkgs,
  inputs,
  ...
}:
let
  inherit (lib) mkOption types;
  cfg = config.melinoe.melnode;
  net = config.melinoe.net;
  cluster = config.melinoe.cluster;

  tunPrefix = "node-";
  controlSocket = "/run/melnode/control.sock";

  # The control plane (userspace), and the data plane: bpfx, a small generic kernel module, and the
  # melnode eBPF object compiled against the types of the running kernel. Routing and protocol
  # logic is in the object, so changing it never needs a new module.
  package = pkgs.buildGoModule {
    pname = "melnode-cp";
    version = "0.1.0";
    src = ./melnode/src;
    subPackages = [
      "cmd/melnode-cp"
      "cmd/melnode-dpctl"
    ];
    vendorHash = "sha256-mu6nUf4cD59wxkxdEy+RjgDJVYJGjB9XhBeypAZHn0I=";
    env.CGO_ENABLED = "0";
    meta.mainProgram = "melnode-cp";
  };
  dataplane = pkgs.callPackage (inputs.melinoe-vpn-ebpf + "/package.nix") {
    bpfx-kernel = inputs.bpfx-kernel.outPath;
    inherit (config.boot.kernelPackages) kernel;
  };

  # What the control plane needs to load and run the data plane (on top of CAP_NET_ADMIN).
  dataplaneCaps = [
    "CAP_BPF"
    "CAP_PERFMON"
    "CAP_SYS_ADMIN"
    "CAP_SYS_RESOURCE"
  ];

  toolPath = [
    pkgs.iproute2
    pkgs.nftables
  ];

  # What melnode dials: an address, without the port (that is cfg.port) or an IPv6 zone. The IPv6
  # pattern only checks the shape, the control plane checks the address itself.
  ipv4Octet = "(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])";
  isIpLiteral =
    host:
    builtins.match "${ipv4Octet}\\.${ipv4Octet}\\.${ipv4Octet}\\.${ipv4Octet}" host != null
    || builtins.match "[0-9a-fA-F]*(:[0-9a-fA-F]*){2,7}" host != null;

  endpointOf =
    name: peer: if peer.endpoint != null then peer.endpoint else cluster.nodes.${name}.endpoint;

  formatEndpoint =
    host:
    if lib.hasInfix ":" host then "[${host}]:${toString cfg.port}" else "${host}:${toString cfg.port}";

  links = lib.sort (a: b: a.peerid < b.peerid) (
    lib.mapAttrsToList (
      name: peer:
      let
        endpoint = endpointOf name peer;
      in
      {
        peerid = cluster.nodes.${name}.id;
        peerPubkey = cluster.nodes.${name}.pubkey;
        inherit (peer) prependCount;
      }
      // lib.optionalAttrs (endpoint != null) { endpoint = formatEndpoint endpoint; }
    ) cfg.peers
  );

  helper = pkgs.writers.writePython3Bin "melnode-helper" { doCheck = false; } (
    builtins.readFile ./melnode/helper.py
  );

  routeMirror = pkgs.writers.writePython3Bin "melinoe-uplink-routes" {
    libraries = [ pkgs.python3Packages.pyroute2 ];
    doCheck = false;
  } (builtins.readFile ./melnode/route-mirror.py);

  mnctl = pkgs.writers.writePython3Bin "mnctl" { doCheck = false; } (
    builtins.readFile ./melnode/mnctl.py
  );

  helperConfig = pkgs.writeText "melnode-helper.json" (
    builtins.toJSON {
      node_id = config.melinoe.node.id;
      table_base = net.outboundMarkBase;
      tun_prefix = tunPrefix;
      control_socket = controlSocket;
      pub_ips = net.publicPrefixes;
      extra_routes = cfg.extraRoutes;
      vm_ifaces = lib.optionals config.melinoe.incus.enable (
        map (vm: vm.iface) (lib.attrValues cluster.vms)
      );
    }
  );

  helperCmd = "${helper}/bin/melnode-helper --config ${helperConfig}";

  hook =
    kind:
    pkgs.writeShellScript "melnode-tun-${kind}-hook" ''
      exec ${helperCmd} hook-${kind} "$@"
    '';

  controlPlaneConfig = (pkgs.formats.toml { }).generate "melnode-cp.toml" (
    {
      localID = config.melinoe.node.id;
      localPort = cfg.port;
      localPrivkeyPath = cfg.privateKeyFile;
      inherit (cfg) mtu introspectIntervalMs;
      inherit tunPrefix;
      fwmark = net.uplinkMark;
      identityPrefix = "${config.melinoe.node.address}/32";
      inherit controlSocket;
      introspectListen = ":${toString cfg.introspectPort}";
      tunCreateHookBin = "${hook "create"}";
      tunDestroyHookBin = "${hook "destroy"}";
      link = links;
    }
    // {
      ebpfObject = "${dataplane}/lib/bpf/melnode.bpf.o";
    }
  );

  service = {
    before = [ "network-online.target" ];
    wantedBy = [ "multi-user.target" ];
    serviceConfig = {
      Restart = "always";
      RestartSec = 1;
      AmbientCapabilities = [ "CAP_NET_ADMIN" ];
      CapabilityBoundingSet = [ "CAP_NET_ADMIN" ];
    };
  };

  afterInetSetup = {
    after = [
      "network.target"
      "melinoe-inet-setup.service"
    ];
    wants = [
      "network.target"
      "melinoe-inet-setup.service"
    ];
  };
in
{
  imports = [ inputs.bpfx-kernel.nixosModules.bpfx ];

  options.melinoe.melnode = {
    enable = mkOption {
      type = types.bool;
      default = true;
      description = "Run the melnode mesh VPN (Noise tunnels, path-vector routing).";
    };

    port = mkOption {
      type = types.port;
      default = 60198;
      description = "UDP port every node listens on and every link dials.";
    };

    introspectPort = mkOption {
      type = types.port;
      default = 60198;
      description = "TCP port of the read-only introspection API, reachable from the other nodes.";
    };

    introspectIntervalMs = mkOption {
      type = types.ints.between 100 60000;
      default = 1000;
      description = "How often the introspection API's snapshot is rebuilt.";
    };

    mtu = mkOption {
      type = types.ints.between 576 65000;
      default = 1416;
      description = "WireGuard's 1420 minus melnode's 4-byte routing header, so a full-size packet fits a 1500-byte underlay.";
    };

    privateKeyFile = mkOption {
      type = types.str;
      default = "/etc/melinoe/wg.privatekey";
    };

    extraRoutes = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "Additional prefixes this node advertises into the mesh.";
    };

    peers = mkOption {
      default = { };
      description = "Links to dial, by node name. A bare number is the prependCount.";
      type = types.attrsOf (
        types.coercedTo types.ints.unsigned (prependCount: { inherit prependCount; }) (
          types.submodule {
            options = {
              endpoint = mkOption {
                type = types.nullOr types.str;
                default = null;
                description = "Overrides the node's own endpoint. If neither is set the link is listen-only.";
              };
              prependCount = mkOption {
                type = types.ints.between 0 254;
                default = 0;
                description = "AS-path prepending on routes relayed over this link; raises its path length to make it less preferred. Paths hold 255 nodes, so 254 is the most that still leaves room for this node itself.";
              };
            };
          }
        )
      );
    };
  };

  config = lib.mkIf cfg.enable {
    boot.bpfx.enable = true;

    assertions = lib.concatLists (
      lib.mapAttrsToList (
        name: peer:
        let
          known = cluster.nodes ? ${name};
          endpoint = if known then endpointOf name peer else null;
        in
        [
          {
            assertion = known;
            message = "melinoe.melnode.peers: ${name} is not in melinoe.cluster.nodes.";
          }
          {
            assertion = name != config.networking.hostName;
            message = "melinoe.melnode.peers: ${name} lists itself as a peer.";
          }
          {
            assertion = endpoint == null || isIpLiteral endpoint;
            message = "melinoe.melnode.peers: the endpoint for ${name} (${toString endpoint}) must be an IPv4 or IPv6 address, without a port or zone: melnode does not resolve hostnames, and always dials ${toString cfg.port}.";
          }
        ]
      ) cfg.peers
    );

    melinoe.firewall = {
      openPorts.udp = [ cfg.port ];
      nodePorts.tcp = [ cfg.introspectPort ];
    };

    environment.systemPackages = [ mnctl ];

    systemd.services = {
      melinoe-uplink-routes = lib.mkMerge [
        service
        {
          description = "Mirror non-mesh routes from the main table into the uplink fwmark table";
          after = [ "network-pre.target" ];
          wants = [ "network-pre.target" ];
          serviceConfig.ExecStart = "${routeMirror}/bin/melinoe-uplink-routes --to-table ${toString net.uplinkMark} --exclude-prefix ${tunPrefix}";
        }
      ];

      melnode-cp = lib.mkMerge [
        service
        afterInetSetup
        {
          description = "melnode control plane (liveness, path-vector routing, host integration)";
          after = [
            "melinoe-uplink-routes.service"
            "nftables.service"
          ];
          wants = [
            "melinoe-uplink-routes.service"
            "nftables.service"
          ];
          stopIfChanged = false;
          restartTriggers = [ (builtins.hashString "sha256" config.networking.nftables.ruleset) ];
          path = toolPath;
          serviceConfig = {
            ExecStart = "${package}/bin/melnode-cp -config ${controlPlaneConfig}";
            RuntimeDirectory = "melnode";
            AmbientCapabilities = dataplaneCaps;
            CapabilityBoundingSet = dataplaneCaps;
          };
        }
      ];

      melnode-helper = lib.mkMerge [
        service
        {
          description = "melnode helper (route advertisement, per-tun policy routing)";
          after = [
            "melnode-cp.service"
            "nftables.service"
          ];
          wants = [ "melnode-cp.service" ];
          stopIfChanged = false;
          path = toolPath;
          serviceConfig.ExecStart = "${helperCmd} daemon";
        }
      ];
    };
  };
}
