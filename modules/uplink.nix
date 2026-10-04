{
  config,
  lib,
  pkgs,
  melinoeNodeIntraIP,
  ...
}:
let
  hostAddr = melinoeNodeIntraIP config.melinoe.node.id;
  netCfg = config.melinoe.node.networking;
  meshCidr = config.melinoe.cluster.networking.containerCidr;
  mark = toString netCfg.uplinkFwMark;
  uplinkGroup = "1";
  uplinkIface =
    idx: uplink:
    if builtins.length uplink.iface > 1 then "bond${toString idx}" else builtins.head uplink.iface;
  mkUplinkScript =
    idx: uplink:
    let
      ifaces = uplink.iface;
      isBonded = builtins.length ifaces > 1;
      iface = uplinkIface idx uplink;
      ipParts = lib.splitString "/" uplink.ip;
      ipAddr = builtins.head ipParts;
      ipPrefixFromIp = if builtins.length ipParts > 1 then builtins.elemAt ipParts 1 else null;
      subnetParts = if uplink.subnet != null then lib.splitString "/" uplink.subnet else null;
      subnetPrefix =
        if subnetParts != null && builtins.length subnetParts > 1 then
          builtins.elemAt subnetParts 1
        else
          null;
      ipWithPrefix =
        if subnetPrefix != null then
          "${ipAddr}/${subnetPrefix}"
        else if ipPrefixFromIp != null then
          uplink.ip
        else
          "${ipAddr}/32";
    in
    ''
      ${lib.optionalString isBonded ''
        ip link add ${iface} type bond mode 802.3ad
        ${lib.optionalString (uplink.lacpRate != null) ''
          ip link set ${iface} type bond lacp_rate ${uplink.lacpRate}
        ''}
        ${lib.concatMapStringsSep "\n" (name: ''
          ip link set ${name} down
          ip link set ${name} master ${iface}
        '') ifaces}
      ''}
      ip link set ${iface} group ${uplinkGroup} up
      ip addr flush dev ${iface}
      ip addr replace ${ipWithPrefix} dev ${iface}
      ${lib.optionalString (uplink.subnet != null) ''
        ip route replace ${uplink.subnet} dev ${iface}
      ''}
    '';
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

    systemd.services.melinoe-inet-setup = lib.mkIf (netCfg.enabled && netCfg.uplinks != [ ]) {
      description = "Configure uplink interfaces for host internet connectivity";
      after = [ "network-pre.target" ];
      wants = [ "network-pre.target" ];
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
          ip addr del ${hostAddr}/32 dev lo >/dev/null 2>&1 || true
          ip addr add ${hostAddr}/32 dev lo
          ip route replace unreachable ${meshCidr}
          sysctl -w net.ipv4.conf.default.rp_filter=0
          sysctl -w net.ipv4.conf.all.rp_filter=0
          ${lib.concatStringsSep "\n" (lib.imap0 mkUplinkScript netCfg.uplinks)}
          ${
            let
              firstUplink = lib.head netCfg.uplinks;
            in
            lib.optionalString (firstUplink.gateway != null) ''
              ip route replace default via ${firstUplink.gateway} dev ${uplinkIface 0 firstUplink}
            ''
          }
        '';
      };
      path = [
        pkgs.iproute2
        pkgs.procps
      ];
    };
  };
}
