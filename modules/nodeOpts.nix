{ lib, config, ... }:
let
  inherit (lib) mkOption types;

  accessRuleType = description: {
    type = types.submodule {
      options = {
        tcp = mkOption {
          type = types.listOf types.port;
          default = [ ];
          description = "TCP ports to allow.";
        };
        udp = mkOption {
          type = types.listOf types.port;
          default = [ ];
          description = "UDP ports to allow.";
        };
        ipProtocols = mkOption {
          type = types.listOf types.ints.u8;
          default = [ ];
          description = "IP protocol numbers to allow (e.g. 4 for IPIP).";
        };
      };
    };
    default = { };
    inherit description;
  };
in
{
  options.melinoe.node = {
    id = mkOption {
      type = types.nullOr types.int;
      default = null;
      description = "Unique node ID used for addressing and routing.";
    };

    regions = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "Region tags for this node.";
    };

    isVMHost = mkOption {
      type = types.bool;
      default = true;
      description = ''
        Whether this node runs Incus and hosts local containers/VMs.

        Disable for cluster nodes that should still be full mesh/BGP members
        (still get gre_ctmark routing, still receive routes to other nodes'
        VMs, still advertise their own pub_ips/extraRoutes via melinoe-route)
        but never run any containers themselves - melinoe-route.nix passes
        --noLocalVMs to the route daemon in that case so it doesn't bother
        scanning for local vm- interfaces that will never exist.
      '';
    };

    isBuildServer = mkOption {
      type = types.bool;
      default = false;
      description = "Whether this node is allowed to act as a build server.";
    };

    remoteBuildOn = mkOption {
      type = types.listOf (
        types.submodule {
          freeformType = types.attrs;
          options = {
            publicHostCA = mkOption {
              type = types.nullOr types.str;
              default = null;
              description = "The exact @cert-authority line to inject into the machine's publicHostKey.";
            };
          };
        }
      );
      default = [ ];
      description = "Wrapper around nix.buildMachines with SSH CA support.";
    };

    isRemoteUpdatable = mkOption {
      type = types.bool;
      default = true;
      description = "Whether to enable the remote update scripts and the gitlab-deploy user allowed to trigger them.";
    };

    serialConsoleMode = mkOption {
      type = types.bool;
      default = false;
      description = "Enable serial console support for EFI/GRUB on this node.";
    };

    legacyBoot = mkOption {
      type = types.bool;
      default = false;
      description = "Enable legacy GRUB boot settings for this node.";
    };

    networking = {
      enabled = mkOption {
        type = types.bool;
        default = true;
        description = "Enable Melinoe-managed networking modules (uplink setup, etc).";
      };

      uplinkVeth = mkOption {
        type = types.str;
        default = "inet0";
        description = ''
          Name of the host-side veth interface for the inet netns pair set up
          by melinoe-uplink-setup.nix. Consumed there (where the interface is
          created) and in nftables.nix (NAT/masquerade rules), so it's an
          option rather than a literal duplicated in both.
        '';
      };

      uplinkFwMark = mkOption {
        type = types.ints.u32;
        default = 51820;
        description = ''
          fwmark value - and, since melinoe-uplink-setup.nix also uses it as
          the policy-routing table id, table number - used to route return
          traffic for the internet uplink back out via uplinkVeth instead
          of over the WireGuard mesh. Consumed by melinoe-wireguard-setup.nix,
          melinoe-uplink-setup.nix, and nftables.nix; kept as a single option
          so those three stay in sync.
        '';
      };

      vmOutboundMarkBase = mkOption {
        type = types.ints.u32;
        default = 1000;
        description = ''
          Base fwmark/route-table id for "route this VM's (or ipip tunnel's)
          traffic via node N" - node N gets mark/table vmOutboundMarkBase + N.
          nftables.nix derives its ct-mark matching range from this plus
          cluster.networking.nodeIdRange.max.

          melinoe-route.nix's embedded Go daemon has a hard dependency on this
          same numbering for its own ipip route-table allocation, but since
          that value is baked into a compiled binary rather than read from
          Nix config, changing this option won't propagate there automatically
          - melinoe-route.nix asserts this option is still 1000 specifically
          to catch that case instead of silently diverging.
        '';
      };

      wireguardBasePort = mkOption {
        type = types.port;
        default = 64512;
        description = ''
          Base UDP port for WireGuard mesh listeners; node N listens on
          wireguardBasePort + N. Equals BGP's ASN base by coincidence, not by any
          actual relationship between the two - free to change independently if
          it ever collides with an upstream firewall or another service.
        '';
      };

      openPorts = mkOption (
        accessRuleType "TCP/UDP ports or IP protocols to accept from any source on the host's own INPUT chain, e.g. ssh/haproxy/incus."
      );
      specialHostAccess = mkOption (
        accessRuleType "TCP/UDP ports or IP protocols to accept from the host range on the host's own INPUT chain, e.g. the melinoe-route protocol."
      );
      specialLoopbackAccess = mkOption (
        accessRuleType "TCP/UDP ports or IP protocols to accept over a WireGuard interface from the node loopback range, e.g. ipip."
      );
      specialWgAccess = mkOption (
        accessRuleType "TCP/UDP ports or IP protocols to accept over a WireGuard interface from the WireGuard subnet, e.g. BGP."
      );
      hostInternalPortAllNet = mkOption (
        accessRuleType "TCP/UDP ports or IP protocols to accept from any of the internal subnets, e.g. iperf3."
      );

      uplinks = mkOption {
        type = types.listOf (
          types.submodule {
            options = {
              ip = mkOption {
                type = types.str;
                description = "Primary IP address for this uplink (use /32 notation).";
              };
              pub_ip = mkOption {
                type = types.nullOr types.str;
                default = null;
                description = "Optional public IP address associated with this uplink.";
              };
              iface = mkOption {
                type = types.listOf types.str;
                description = "Interface names that carry the internet uplink; if multiple interfaces are specified, they will be LACP bonded.";
              };
              bondMode = mkOption {
                type = types.nullOr types.str;
                default = null;
                description = "Bonding mode for a multi-interface uplink (currently only \"lacp\" is supported).";
              };
              lacpRate = mkOption {
                type = types.nullOr types.str;
                default = null;
                description = "LACP rate for a bonded uplink (e.g., \"fast\" or \"slow\").";
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
          }
        );
        default = [ ];
        description = "Internet uplink definitions; each entry describes an IP, interface, and optional subnet/gateway.";
      };

      peers = mkOption {
        type = types.listOf (
          types.submodule {
            options = {
              id = mkOption {
                type = types.int;
                description = "Peer node ID (matches wg-keys mapping).";
              };
              endpoint = mkOption {
                type = types.nullOr types.str;
                default = null;
                description = "Override endpoint hostname/IP for this peer; if null, nodePublicInfo.<id>.defaultEndpoint is used.";
              };
              allowedIPs = mkOption {
                type = types.listOf types.str;
                default = [
                  config.melinoe.cluster.networking.wireguardCidr
                  config.melinoe.cluster.networking.bgpCidr
                ];
                description = "Allowed IPs to route via this peer.";
              };
              persistentKeepalive = mkOption {
                type = types.int;
                default = 25;
                description = "Persistent keepalive in seconds.";
              };
            };
          }
        );
        default = [ ];
        description = "WireGuard peers; used to render interfaces and derive BGP neighbors.";
      };
    };
  };
}
