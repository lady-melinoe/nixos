{
  config,
  lib,
  pkgs,
  ...
}:
let
  netCfg = config.melinoe.node.networking;
  meshCidr = config.melinoe.cluster.networking.containerCidr;
  mark = toString netCfg.uplinkFwMark;
  uplinkGroup = 1;
  uplinksEnabled = netCfg.enabled && netCfg.uplinks != [ ];
  uplinkIface =
    idx: uplink:
    if builtins.length uplink.iface > 1 then "bond${toString idx}" else builtins.head uplink.iface;
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
      iface = uplinkIface idx uplink;
      isPrimary = idx == 0 && uplink.gateway != null;
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
          address = [ (uplinkAddress uplink) ];
          gateway = lib.optional isPrimary uplink.gateway;
          linkConfig = {
            Group = uplinkGroup;
            RequiredForOnline = if isPrimary then "yes" else "no";
          };
          networkConfig = {
            ConfigureWithoutCarrier = true;
            IgnoreCarrierLoss = true;
            IPv6AcceptRA = false;
          };
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
  uplinkUnits = lib.imap0 mkUplinkUnits netCfg.uplinks;
in
{
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
          message = "melinoe.node.networking.uplinks: bondMode must be set when multiple interfaces are specified.";
        }
        {
          assertion = !(hasBondMode && entry.bondMode != "lacp");
          message = "melinoe.node.networking.uplinks: only bondMode = \"lacp\" is supported.";
        }
        {
          assertion = !(hasLacpRate && !isBonded);
          message = "melinoe.node.networking.uplinks: lacpRate is only valid when multiple interfaces are specified.";
        }
      ]
    ) netCfg.uplinks;

    systemd.network = lib.mkIf uplinksEnabled {
      enable = true;
      config.networkConfig = {
        ManageForeignRoutes = false;
        ManageForeignRoutingPolicyRules = false;
      };
      wait-online.timeout = 30;
      netdevs = lib.mkMerge (map (units: units.netdevs) uplinkUnits);
      networks = lib.mkMerge (map (units: units.networks) uplinkUnits);
    };

    boot.kernel.sysctl = lib.mkIf uplinksEnabled {
      "net.ipv4.conf.all.rp_filter" = 0;
      "net.ipv4.conf.default.rp_filter" = 0;
      "net.ipv4.conf.*.rp_filter" = 0;
    };

    systemd.services.melinoe-inet-setup = lib.mkIf uplinksEnabled {
      description = "Configure policy routing around the uplink interfaces";
      after = [
        "network-pre.target"
        "systemd-networkd-wait-online.service"
      ];
      wants = [
        "network-pre.target"
        "systemd-networkd-wait-online.service"
      ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = pkgs.writeShellScript "melinoe-inet-setup" ''
          ip rule del pref 0 fwmark ${mark} lookup main suppress_ifgroup default >/dev/null 2>&1 || true
          ip rule add pref 0 fwmark ${mark} lookup main suppress_ifgroup default
          ip rule del pref 1 from all lookup local >/dev/null 2>&1 || true
          ip rule add pref 1 from all lookup local
          ip rule del pref 0 from all lookup local >/dev/null 2>&1 || true
          ip route replace unreachable ${meshCidr}
        '';
      };
      path = [ pkgs.iproute2 ];
    };
  };
}
