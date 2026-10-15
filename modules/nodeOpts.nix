{
  lib,
  config,
  melinoeLib,
  ...
}:
let
  inherit (lib) mkOption types;

  inherit (melinoeLib) accessRuleType;
in
{
  options.melinoe.node = {
    id = mkOption {
      type = types.nullOr types.int;
      default = null;
      description = "Unique node ID used for addressing and routing.";
    };

    networking = {
      intraIP = mkOption {
        type = types.str;
        readOnly = true;
        default = melinoeLib.ip.nodeAddress config.melinoe.cluster.networking.hostCidr config.melinoe.node.id;
        defaultText = lib.literalExpression "melinoeLib.ip.nodeAddress config.melinoe.cluster.networking.hostCidr config.melinoe.node.id";
        description = "This node's own address on melinoe.cluster.networking.hostCidr, derived from melinoe.node.id.";
      };

      enabled = mkOption {
        type = types.bool;
        default = true;
        description = "Enable Melinoe-managed networking modules (uplink setup, etc).";
      };

      uplinkFwMark = mkOption {
        type = types.ints.u32;
        default = 51820;
        description = ''
          fwmark value used to route return traffic for the internet
          uplink(s) back out via the uplink interfaces instead of over the
          mesh. Also the id of the routing table marked packets are looked up
          in, which melinoe-uplink-routes keeps as a copy of the main table
          minus routes via melnode tuns. Also set as melnode's socket
          fwmark so its own UDP traffic stays on the uplink. Consumed by
          melnode.nix, net/uplink.nix, and nftables.nix; kept as a single
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

      peers = mkOption {
        type = types.listOf (
          types.submodule {
            options = {
              id = mkOption {
                type = types.int;
                description = "Peer node ID (must have a melinoe.other.nodePublicInfo entry).";
              };
              endpoint = mkOption {
                type = types.nullOr types.str;
                default = null;
                description = "Override endpoint IP for this peer (melnode takes an IP literal, no hostnames); if null, melinoe.other.nodePublicInfo.<id>.defaultEndpoint is used. If that is null too, the link is listen-only: this node never dials the peer and waits for it to connect in.";
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
