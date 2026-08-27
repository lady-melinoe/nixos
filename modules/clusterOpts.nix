{ lib, config, ... }:
let
  inherit (lib) mkOption types;
  addr = config.melinoe.cluster.networking;

  pow2 = n: if n == 0 then 1 else 2 * pow2 (n - 1);
  mod = a: b: a - (a / b) * b;

  ip4ToInt = ip: lib.foldl' (acc: octet: acc * 256 + lib.toInt octet) 0 (lib.splitString "." ip);

  int4ToIp =
    n:
    lib.concatStringsSep "." (
      map (shift: toString (mod (n / pow2 shift) 256)) [
        24
        16
        8
        0
      ]
    );

  parseCidr =
    cidr:
    let
      parts = lib.splitString "/" cidr;
      ipInt = ip4ToInt (lib.elemAt parts 0);
      prefixLength = lib.toInt (lib.elemAt parts 1);
      hostBits = 32 - prefixLength;
      blockSize = pow2 hostBits;
      networkInt = ipInt - (mod ipInt blockSize);
    in
    if networkInt != ipInt then
      throw "melinoe.cluster.networking: ${cidr} is not aligned to its /${toString prefixLength} network base (did you mean ${int4ToIp networkInt}/${toString prefixLength}?)"
    else
      {
        inherit prefixLength hostBits blockSize;
        network = networkInt;
      };

  smallestBlockSize = lib.foldl' lib.min (1 * 256 * 256 * 256) (
    map (cidr: (parseCidr cidr).blockSize) [
      addr.hostCidr
      addr.wireguardCidr
      addr.bgpCidr
    ]
  );

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
          description = "Highest valid melinoe.node.id. Defaults to the capacity of the smallest of hostCidr/wireguardCidr/bgpCidr, minus the two ends reserved for network/broadcast-shaped special addresses.";
        };
      };

      hostCidr = mkOption {
        type = types.str;
        default = "198.18.0.0/24";
        description = ''
          Per-node "host"/intra address range. Node N's own address on this
          range (see melinoeNodeIntraIP) is used for haproxy sourcing and
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

      wireguardCidr = mkOption {
        type = types.str;
        default = "198.19.3.0/24";
        description = "The WireGuard mesh subnet. Node N's WireGuard address is given by melinoeNodeWgIP N.";
      };

      bgpCidr = mkOption {
        type = types.str;
        default = "198.51.100.0/24";
        description = ''
          Node loopback range, used as BGP router-ids and ipip tunnel endpoints.
          Node N's loopback address is given by melinoeNodeLoopbackIP N.
        '';
      };
    };
  };
}
