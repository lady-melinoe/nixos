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

        Disable for cluster nodes that should still be full mesh members
        (still receive routes to other nodes' VMs, still advertise their own
        pub_ips/extraRoutes via melnode) but never run any containers
        themselves - melnode.nix doesn't bother scanning for local vm-
        interfaces in that case, since they will never exist.
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

      uplinkFwMark = mkOption {
        type = types.ints.u32;
        default = 51820;
        description = ''
          fwmark value - and, since uplink.nix also uses it as
          the policy-routing table id, table number - used to route return
          traffic for the internet uplink(s) back out via the uplink
          interfaces instead of over the mesh. Also set as melnode's socket
          fwmark so its own UDP traffic stays on the uplink. Consumed by
          melnode.nix, uplink.nix, and nftables.nix; kept as a single
          option so those three stay in sync.
        '';
      };

      vmOutboundMarkBase = mkOption {
        type = types.ints.u32;
        default = 1000;
        description = ''
          Base fwmark/route-table id for "route this VM's (or mesh tun's)
          traffic via node N" - node N gets mark/table vmOutboundMarkBase + N.
          nftables.nix derives its ct-mark matching range from this plus
          cluster.networking.nodeIdRange.max.

          melnode.nix passes this to melnode-helper (table_base in its JSON
          config), which sets up the per-node tables and nft entries from
          melnode's tun create/destroy hooks, so changing this option
          propagates there automatically.
        '';
      };

      openPorts = mkOption (
        accessRuleType "TCP/UDP ports or IP protocols to accept from any source on the host's own INPUT chain, e.g. ssh/haproxy/incus."
      );
      specialHostAccess = mkOption (
        accessRuleType "TCP/UDP ports or IP protocols to accept from the host range on the host's own INPUT chain, e.g. the Incus cluster port."
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
                description = "Peer node ID (must have a melinoe.nodePublicInfo entry).";
              };
              endpoint = mkOption {
                type = types.nullOr types.str;
                default = null;
                description = "Override endpoint IP for this peer (melnode takes an IP literal, no hostnames); if null, nodePublicInfo.<id>.defaultEndpoint is used.";
              };
              prependCount = mkOption {
                type = types.ints.unsigned;
                default = 0;
                description = ''
                  Path-vector AS-prepending for routes relayed out over this
                  link (melnode [[link]] prependCount). Raises the path length
                  anyone downstream sees for routes relayed this way, making
                  this link less preferred: the traffic-engineering lever that
                  replaces region preference. 0 is plain shortest-path.
                '';
              };
            };
          }
        );
        default = [ ];
        description = "melnode links: one Noise tunnel per entry, all on melinoe.services.melnode.port.";
      };
    };
  };
}
