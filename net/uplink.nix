{
  config,
  lib,
  pkgs,
  ...
}:
let
  inherit (lib) mkOption types;
  cfg = config.melinoe.networking.uplinks;
  meshCidr = config.melinoe.cluster.networking.containerCidr;
  meshAddress = config.melinoe.node.networking.intraIP;
  # Owned by melnode/nftables, not by uplink setup itself; this just has to
  # agree with them on which mark return traffic gets routed by.
  mark = toString config.melinoe.node.networking.uplinkFwMark;
  hasWifiUplink = lib.any (uplink: uplink.wifi) cfg.interfaces;
  uplinkAddress =
    uplink:
    let
      ipParts = lib.splitString "/" uplink.ip;
      ipAddr = builtins.head ipParts;
      ipPrefixFromIp = if builtins.length ipParts > 1 then builtins.elemAt ipParts 1 else null;
      subnetParts = if uplink.subnet != null then lib.splitString "/" uplink.subnet else null;
      subnetPrefix =
        if subnetParts != null && builtins.length subnetParts > 1 then
          builtins.elemAt subnetParts 1
        else
          null;
    in
    if subnetPrefix != null then
      "${ipAddr}/${subnetPrefix}"
    else if ipPrefixFromIp != null then
      uplink.ip
    else
      "${ipAddr}/32";
  mkUplinkUnits =
    idx: uplink:
    let
      ifaces = uplink.iface;
      isBonded = builtins.length ifaces > 1;
      iface = builtins.elemAt cfg.ifaceNames idx;
      isPrimary = idx == 0 && uplink.gateway != null;
      staticNetworkConfig = {
        ConfigureWithoutCarrier = true;
        IgnoreCarrierLoss = true;
      };
    in
    {
      netdevs = lib.optionalAttrs isBonded {
        "10-${iface}" = {
          netdevConfig = {
            Name = iface;
            Kind = "bond";
          };
          bondConfig = {
            Mode = "802.3ad";
          }
          // lib.optionalAttrs (uplink.lacpRate != null) { LACPTransmitRate = uplink.lacpRate; };
        };
      };
      networks = {
        "10-${iface}" = {
          matchConfig.Name = iface;
          address = lib.optional (!uplink.dhcp) (uplinkAddress uplink);
          gateway = lib.optional isPrimary uplink.gateway;
          dhcpV4Config = lib.mkIf uplink.dhcp {
            UseDNS = false;
            UseDomains = false;
          };
          linkConfig = {
            RequiredForOnline = if isPrimary then "yes" else "no";
          };
          networkConfig = {
            IPv6AcceptRA = false;
          }
          // (if uplink.dhcp then { DHCP = "ipv4"; } else staticNetworkConfig);
        };
      }
      // lib.optionalAttrs isBonded (
        lib.listToAttrs (
          map (
            member:
            lib.nameValuePair "10-${member}" {
              matchConfig.Name = member;
              networkConfig.Bond = iface;
              linkConfig.RequiredForOnline = "no";
            }
          ) ifaces
        )
      );
    };
  uplinkUnits = lib.imap0 mkUplinkUnits cfg.interfaces;
in
{
  options.melinoe.networking.uplinks = {
    enabled = mkOption {
      type = types.bool;
      default = false;
      description = "Enable Melinoe-managed internet uplink setup (policy routing, bonding, optional wifi).";
    };

    interfaces = mkOption {
      type = types.listOf (
        types.submodule {
          options = {
            dhcp = mkOption {
              type = types.bool;
              default = false;
              description = "Get this uplink's address and default route over DHCPv4 instead of ip/subnet/gateway.";
            };
            ip = mkOption {
              type = types.nullOr types.str;
              default = null;
              description = "Primary IP address for this uplink (use /32 notation). Required unless dhcp is set.";
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
            wifi = mkOption {
              type = types.bool;
              default = false;
              description = "Whether this uplink's interface is a wifi device managed by iwd rather than a wired link.";
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
      description = "Internet uplink definitions; each entry describes an IP, interface, and optional subnet/gateway/wifi.";
    };

    ifaceNames = mkOption {
      type = types.listOf types.str;
      readOnly = true;
      default = lib.imap0 (
        idx: uplink:
        if builtins.length uplink.iface > 1 then "bond${toString idx}" else builtins.head uplink.iface
      ) cfg.interfaces;
      defaultText = lib.literalMD "one name per uplink: its interface, or `bond<index>` when it has several";
      description = "Interface name each entry of interfaces ends up on, in the same order.";
    };

    pubIps = mkOption {
      type = types.listOf types.str;
      readOnly = true;
      default = lib.filter (ip: ip != null) (map (entry: entry.pub_ip or null) cfg.interfaces);
      defaultText = lib.literalMD "every non-null `pub_ip` in interfaces";
      description = "Public IPs of this node's uplinks.";
    };
  };

  config = {
    assertions = lib.concatMap (
      entry:
      let
        isBonded = builtins.length entry.iface > 1;
        hasBondMode = entry.bondMode != null;
        hasLacpRate = entry.lacpRate != null;
      in
      [
        {
          assertion = !(isBonded && !hasBondMode);
          message = "melinoe.networking.uplinks.interfaces: bondMode must be set when multiple interfaces are specified.";
        }
        {
          assertion = !(hasBondMode && entry.bondMode != "lacp");
          message = "melinoe.networking.uplinks.interfaces: only bondMode = \"lacp\" is supported.";
        }
        {
          assertion = entry.dhcp || entry.ip != null;
          message = "melinoe.networking.uplinks.interfaces: ip must be set unless dhcp = true.";
        }
        {
          assertion = !entry.dhcp || (entry.ip == null && entry.subnet == null && entry.gateway == null);
          message = "melinoe.networking.uplinks.interfaces: ip, subnet and gateway come from DHCP when dhcp = true; leave them unset.";
        }
        {
          assertion = !(hasLacpRate && !isBonded);
          message = "melinoe.networking.uplinks.interfaces: lacpRate is only valid when multiple interfaces are specified.";
        }
        {
          assertion = !(entry.wifi && isBonded);
          message = "melinoe.networking.uplinks.interfaces: wifi uplinks don't support bonding.";
        }
      ]
    ) cfg.interfaces
    ++ [
      {
        assertion = !cfg.enabled || config.melinoe.node.networking.enabled;
        message = "melinoe.node.networking.enabled must be true when melinoe.networking.uplinks.enabled is true.";
      }
    ];

    networking.wireless.iwd = lib.mkIf hasWifiUplink {
      enable = true;
      settings = {
        General.EnableNetworkConfiguration = false;
        DriverQuirks.DefaultInterface = "*";
      };
    };

    networking.useDHCP = lib.mkIf cfg.enabled false;

    systemd.network = lib.mkMerge [
      {
        networks."10-lo" = {
          matchConfig.Name = "lo";
          address = [ "${meshAddress}/32" ];
          networkConfig.KeepConfiguration = "static";
        };
      }
      (lib.mkIf cfg.enabled {
        enable = true;
        wait-online.timeout = 30;
        config.networkConfig = {
          ManageForeignRoutes = false;
          ManageForeignRoutingPolicyRules = false;
        };
        netdevs = lib.mkMerge (map (units: units.netdevs) uplinkUnits);
        networks = lib.mkMerge (map (units: units.networks) uplinkUnits);
      })
    ];

    boot.kernel.sysctl = lib.mkIf cfg.enabled {
      "net.ipv4.conf.all.rp_filter" = 0;
      "net.ipv4.conf.default.rp_filter" = 0;
      "net.ipv4.conf.*.rp_filter" = 0;
    };

    systemd.services.melinoe-inet-setup = lib.mkIf cfg.enabled {
      description = "Configure policy routing around the uplink interfaces";
      # Only installs ip rules/routes, which don't need any link to be up.
      after = [ "network-pre.target" ];
      wants = [ "network-pre.target" ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = pkgs.writeShellScript "melinoe-inet-setup" ''
          ip rule del pref 0 fwmark ${mark} lookup ${mark} >/dev/null 2>&1 || true
          ip rule add pref 0 fwmark ${mark} lookup ${mark}
          ip rule del pref 1 from all lookup local >/dev/null 2>&1 || true
          ip rule add pref 1 from all lookup local
          ip rule del pref 0 from all lookup local >/dev/null 2>&1 || true
          ip rule del pref 2 fwmark 0xca6c unreachable >/dev/null 2>&1 || true
          ip rule add pref 2 fwmark 0xca6c unreachable
          ip route replace unreachable ${meshCidr}
        '';
      };
      path = [ pkgs.iproute2 ];
    };
  };
}
