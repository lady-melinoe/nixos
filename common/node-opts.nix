{ lib, config, ... }:
let
  inherit (lib) mkOption types;
in {
  options.melinoe = {
    nodeId = mkOption {
      type = types.nullOr types.int;
      default = null;
      description = "Unique node ID used for addressing and routing.";
    };

    underlayPrefix = mkOption {
      type = types.str;
      default = "198.19.0";
      description = "Underlay /24 prefix (three octets) used for p2p addressing, e.g., 198.19.0 or 198.19.1.";
    };

    incusDefaultStorageSource = mkOption {
      type = types.nullOr types.str;
      default = "/var/lib/incus/storage-pools/default";
      description = "Source path for the Incus default storage pool (e.g., /var/lib/incus/storage-pools/default).";
    };

    bgpPeers = mkOption {
      type = types.listOf (types.submodule {
        options = {
          id = mkOption {
            type = types.int;
            description = "Peer node ID (used to derive remote AS as 64512 + id by default).";
          };
          addr = mkOption {
            type = types.str;
            description = "Peer IPv4 address on the underlay (198.19.0.X, but not assumed).";
          };
        };
      });
      default = [ ];
      description = "List of BGP peers with node ID and underlay IPv4 address.";
    };

    inetIfs = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "Interface names (or patterns) used by nftables (e.g., eno1, bond0).";
    };

    wgPorts = mkOption {
      type = types.listOf types.int;
      default = [ ];
      description = "UDP ports to allow for WireGuard (derived automatically when using the wireguard module).";
    };

    wgPeers = mkOption {
      type = types.listOf (types.submodule {
        options = {
          id = mkOption {
            type = types.int;
            description = "Peer node ID (matches wg-keys mapping).";
          };
          endpoint = mkOption {
            type = types.str;
            description = "Endpoint hostname/IP for this peer; port is derived automatically.";
          };
          allowedIPs = mkOption {
            type = types.listOf types.str;
            description = "Allowed IPs to route via this peer.";
          };
          persistentKeepalive = mkOption {
            type = types.int;
            default = 25;
            description = "Persistent keepalive in seconds.";
          };
        };
      });
      default = [ ];
      description = "WireGuard peers; used to render interfaces and auto-extend BGP peers.";
    };
  };

  config.assertions = [
    {
      assertion = config.melinoe.nodeId != null;
      message = "melinoe.nodeId must be set for this host.";
    }
    {
      assertion = config.melinoe.inetIfs != [ ];
      message = "melinoe.inetIfs must be set for this host.";
    }
  ];
}
