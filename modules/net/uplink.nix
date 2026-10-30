{
  config,
  lib,
  pkgs,
  ...
}:
let
  inherit (lib) mkOption types;
  cfg = config.melinoe.uplinks;
  net = config.melinoe.net;
  enabled = cfg != [ ];

  mark = toString net.uplinkMark;
  markHex = "0x${lib.toLower (lib.toHexString net.uplinkMark)}";

  endpoint = config.melinoe.cluster.nodes.${config.networking.hostName}.endpoint;

  uplinkNames = lib.imap0 (
    idx: uplink:
    if lib.length uplink.interfaces > 1 then "bond${toString idx}" else lib.head uplink.interfaces
  ) cfg;

  mkUnits =
    idx: uplink:
    let
      name = lib.elemAt uplinkNames idx;
      bonded = lib.length uplink.interfaces > 1;
      primary = idx == 0 && uplink.gateway != null;
    in
    {
      netdevs = lib.optionalAttrs bonded {
        "10-${name}" = {
          netdevConfig = {
            Name = name;
            Kind = "bond";
          };
          bondConfig = {
            Mode = "802.3ad";
          }
          // lib.optionalAttrs (uplink.lacpRate != null) { LACPTransmitRate = uplink.lacpRate; };
        };
      };

      networks = {
        "10-${name}" = {
          matchConfig.Name = name;
          address = lib.optional (!uplink.dhcp) uplink.address;
          gateway = lib.optional primary uplink.gateway;
          dhcpV4Config = lib.mkIf uplink.dhcp {
            UseDNS = false;
            UseDomains = false;
          };
          linkConfig.RequiredForOnline = if primary then "yes" else "no";
          networkConfig = {
            IPv6AcceptRA = false;
          }
          // (
            if uplink.dhcp then
              { DHCP = "ipv4"; }
            else
              {
                ConfigureWithoutCarrier = true;
                IgnoreCarrierLoss = true;
              }
          );
        };
      }
      // lib.optionalAttrs bonded (
        lib.listToAttrs (
          map (
            member:
            lib.nameValuePair "10-${member}" {
              matchConfig.Name = member;
              networkConfig.Bond = name;
              linkConfig.RequiredForOnline = "no";
            }
          ) uplink.interfaces
        )
      );
    };

  units = lib.imap0 mkUnits cfg;

  assertionsFor =
    uplink:
    let
      prefix = "melinoe.uplinks (${lib.concatStringsSep "+" uplink.interfaces}):";
      bonded = lib.length uplink.interfaces > 1;
    in
    [
      {
        assertion = !bonded || uplink.bondMode != null;
        message = "${prefix} bondMode must be set when there are several interfaces.";
      }
      {
        assertion = uplink.lacpRate == null || bonded;
        message = "${prefix} lacpRate is only valid when there are several interfaces.";
      }
      {
        assertion = uplink.dhcp || uplink.address != null;
        message = "${prefix} address must be set unless dhcp is enabled.";
      }
      {
        assertion = !uplink.dhcp || (uplink.address == null && uplink.gateway == null);
        message = "${prefix} address and gateway come from DHCP when dhcp is enabled.";
      }
      {
        assertion = !(uplink.wifi && bonded);
        message = "${prefix} wifi uplinks can't be bonded.";
      }
    ];
in
{
  options.melinoe = {
    uplinks = mkOption {
      default = [ ];
      description = "Internet uplinks. The first one is the primary, if it has a gateway.";
      type = types.listOf (
        types.submodule {
          options = {
            interfaces = mkOption {
              type = types.listOf types.str;
              description = "Several interfaces are bonded together.";
            };
            dhcp = mkOption {
              type = types.bool;
              default = false;
            };
            address = mkOption {
              type = types.nullOr types.str;
              default = null;
              description = "Address with prefix length.";
            };
            gateway = mkOption {
              type = types.nullOr types.str;
              default = null;
            };
            publicIp = mkOption {
              type = types.nullOr types.str;
              default = endpoint;
              description = "Public address, if different from address (defaults to the node's endpoint).";
            };
            wifi = mkOption {
              type = types.bool;
              default = false;
              description = "The interface is a wifi device managed by iwd.";
            };
            bondMode = mkOption {
              type = types.nullOr (types.enum [ "lacp" ]);
              default = null;
            };
            lacpRate = mkOption {
              type = types.nullOr types.str;
              default = null;
            };
          };
        }
      );
    };

    net = {
      uplinkNames = mkOption {
        type = types.listOf types.str;
        readOnly = true;
        default = uplinkNames;
        description = "Interface each uplink ends up on: its own, or bond<index> when bonded.";
      };
      publicPrefixes = mkOption {
        type = types.listOf types.str;
        readOnly = true;
        default = map (uplink: "${uplink.publicIp}/32") (lib.filter (uplink: uplink.publicIp != null) cfg);
      };
    };
  };

  config = {
    assertions = lib.concatMap assertionsFor cfg;

    networking.wireless.iwd = lib.mkIf (lib.any (uplink: uplink.wifi) cfg) {
      enable = true;
      settings = {
        General.EnableNetworkConfiguration = false;
        DriverQuirks.DefaultInterface = "*";
      };
    };

    networking.useDHCP = lib.mkIf enabled false;

    systemd.network = lib.mkMerge [
      {
        networks."10-lo" = {
          matchConfig.Name = "lo";
          address = [ "${config.melinoe.node.address}/32" ];
          networkConfig.KeepConfiguration = "static";
        };
      }
      (lib.mkIf enabled {
        enable = true;
        wait-online.timeout = 30;
        config.networkConfig = {
          ManageForeignRoutes = false;
          ManageForeignRoutingPolicyRules = false;
        };
        netdevs = lib.mkMerge (map (u: u.netdevs) units);
        networks = lib.mkMerge (map (u: u.networks) units);
      })
    ];

    boot.kernel.sysctl = lib.mkIf enabled {
      "net.ipv4.conf.all.rp_filter" = 0;
      "net.ipv4.conf.default.rp_filter" = 0;
      "net.ipv4.conf.*.rp_filter" = 0;
    };

    systemd.services.melinoe-inet-setup = lib.mkIf enabled {
      description = "Configure policy routing around the uplink interfaces";
      after = [ "network-pre.target" ];
      wants = [ "network-pre.target" ];
      wantedBy = [ "multi-user.target" ];
      path = [ pkgs.iproute2 ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = pkgs.writeShellScript "melinoe-inet-setup" ''
          ip rule del pref 0 fwmark ${mark} lookup ${mark} >/dev/null 2>&1 || true
          ip rule add pref 0 fwmark ${mark} lookup ${mark}
          ip rule del pref 1 from all lookup local >/dev/null 2>&1 || true
          ip rule add pref 1 from all lookup local
          ip rule del pref 0 from all lookup local >/dev/null 2>&1 || true
          ip rule del pref 2 fwmark ${markHex} unreachable >/dev/null 2>&1 || true
          ip rule add pref 2 fwmark ${markHex} unreachable
          ip route replace unreachable ${config.melinoe.cluster.networks.mesh}
        '';
      };
    };
  };
}
