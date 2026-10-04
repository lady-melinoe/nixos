{
  lib,
  config,
  melinoeLib,
  ...
}:
let
  inherit (lib) mkOption types;
  addr = config.melinoe.cluster.networking;

  smallestBlockSize = lib.foldl' lib.min (1 * 256 * 256 * 256) (
    map (cidr: (melinoeLib.ip.parseCidr cidr).blockSize) [
      addr.hostCidr
    ]
  );

  inherit (melinoeLib) accessRuleType;
in
{
  options.melinoe.cluster = {
    virtualMachines = mkOption {
      type = types.listOf (
        types.submodule {
          options = {
            iface = mkOption {
              type = types.str;
              description = "Network interface name for this VM (e.g. \"vm-npm\").";
            };
            ip = mkOption {
              type = types.str;
              description = "IP address for this VM; use CIDR notation for subnet bridges (e.g. \"198.18.3.0/24\"), plain address otherwise.";
            };
            tcp = mkOption {
              type = types.listOf types.port;
              default = [ ];
              description = "TCP ports to NAT/forward to this VM.";
            };
            udp = mkOption {
              type = types.listOf types.port;
              default = [ ];
              description = "UDP ports to NAT/forward to this VM.";
            };
            outbound-via-node = mkOption {
              type = types.nullOr types.int;
              default = null;
              description = "If set, route this VM's outbound traffic via the specified node ID.";
            };
            specialHostAccess = mkOption (
              accessRuleType "TCP/UDP ports or IP protocols this VM is allowed to reach on the host itself (any node's own INPUT chain), e.g. polling glances."
            );
            outboundDropIP = mkOption {
              type = types.listOf types.str;
              default = [ ];
              description = "IP addresses/CIDRs this VM's outbound traffic is blocked from reaching.";
            };
            outboundAllowOnlyIP = mkOption {
              type = types.listOf types.str;
              default = [ ];
              description = "If non-empty, restricts this VM's outbound traffic to only these IP addresses/CIDRs (everything else is dropped).";
            };
          };
        }
      );
      default = [ ];
      description = "Virtual machine network definitions; drives interface setup, NAT rules, and firewall forwarding in nftables.nix.";
    };

    networking = {
      nodeIdRange = {
        min = mkOption {
          type = types.int;
          default = 1;
          description = "Lowest valid melinoe.node.id.";
        };
        max = mkOption {
          type = types.int;
          default = smallestBlockSize - 2;
          description = "Highest valid melinoe.node.id. Defaults to the capacity of hostCidr, minus the two ends reserved for network/broadcast-shaped special addresses.";
        };
      };

      hostCidr = mkOption {
        type = types.str;
        default = "198.18.0.0/24";
        description = ''
          Per-node "host"/intra address range. Node N's own address on this
          range (see melinoe.node.networking.intraIP) is used for haproxy sourcing and
          the firewall's own-node range.
        '';
      };

      containerCidr = mkOption {
        type = types.str;
        default = "198.18.0.0/16";
        description = ''
          Full range containers/VMs live in, containing hostCidr as a subset.
          Traffic from vm interfaces claiming a source address outside this
          range (or inside hostCidr) is dropped by nftables.nix.
        '';
      };

      containerHostAddress = mkOption {
        type = types.str;
        default = "198.18.255.255";
        description = ''
          The "my host" special address reserved out of containerCidr: intended
          for containers/VMs to use as their uplink to reach the node itself.
          Not yet wired up to anything - reserved here so containerCidr's
          documented exclusions (hostCidr, this address) stay accurate as
          that lands.
        '';
      };
    };
  };
}
