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

    incusDefaultStorageSource = mkOption {
      type = types.nullOr types.str;
      default = "/var/lib/incus/storage-pools/default";
      description = "Source path for the Incus default storage pool (e.g., /var/lib/incus/storage-pools/default).";
    };

    inetIfs = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "Interface names (or patterns) used by nftables (e.g., eno1, bond0); may be empty.";
    };

    internet = mkOption {
      type = types.listOf (types.submodule {
        options = {
          ip = mkOption {
            type = types.str;
            description = "Primary IP address for this uplink (use /32 notation).";
          };
          iface = mkOption {
            type = types.listOf types.str;
            description = "Interface names that carry the internet uplink; if multiple interfaces are specified, they will be LACP bonded.";
          };
          subnet = mkOption {
            type = types.nullOr types.str;
            default = null;
            description = "Subnet for the uplink if applicable.";
          };
          gateway = mkOption {
            type = types.nullOr types.str;
            default = null;
            description = "Gateway address if needed for the uplink.";
          };
        };
      });
      default = [ ];
      description = "Internet uplink definitions; each entry describes an IP, interface, and optional subnet/gateway.";
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
            default = [ "198.19.3.0/24" "198.51.100.0/24" ];
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
      description = "WireGuard peers; used to render interfaces and derive BGP neighbors.";
    };
  };

  config.assertions = [
    {
      assertion = config.melinoe.nodeId != null;
      message = "melinoe.nodeId must be set for this host.";
    }
  ];
}
