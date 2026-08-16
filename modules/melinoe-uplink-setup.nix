{
  config,
  lib,
  pkgs,
  melinoeNodeIntraIP,
  ...
}:
let
  cfg = config.melinoe;
  netCfg = config.melinoe.node.networking;
  addr = config.melinoe.cluster.networking;
  nodeID = cfg.node.id;
  hostAddr = melinoeNodeIntraIP nodeID;
  uplinkAddr = addr.hostUplinkAddress;
  ns = "ip netns exec inet";
  uplinkIface =
    idx: uplink:
    if builtins.length uplink.iface > 1 then "bond${toString idx}" else builtins.head uplink.iface;
  uplinkIpAddr = uplink: builtins.head (lib.splitString "/" uplink.ip);
  inetNftRuleset = pkgs.writeText "melinoe-inet.nft" ''
    table ip nat {
      chain prerouting {
        type nat hook prerouting priority dstnat; policy accept;
        ${lib.concatStringsSep "\n" (
          lib.imap0 (
            idx: uplink:
            ''iifname "${uplinkIface idx uplink}" ip daddr ${uplinkIpAddr uplink} dnat to ${hostAddr}''
          ) netCfg.uplinks
        )}
      }
      chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        ${lib.concatStringsSep "\n" (
          lib.imap0 (idx: uplink: ''oifname "${uplinkIface idx uplink}" masquerade'') netCfg.uplinks
        )}
      }
    }
  '';
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
      ${lib.concatMapStringsSep "\n" (name: ''
        ip link set ${name} down
        ip link set ${name} netns inet
      '') ifaces}
      ${lib.optionalString isBonded ''
        ${ns} ip link add ${iface} type bond mode 802.3ad
        ${lib.optionalString (uplink.lacpRate != null) ''
          ${ns} ip link set ${iface} type bond lacp_rate ${uplink.lacpRate}
        ''}
        ${lib.concatMapStringsSep "\n" (name: ''
          ${ns} ip link set ${name} master ${iface}
        '') ifaces}
      ''}
      ${ns} ip link set ${iface} up
      ${ns} ip addr flush dev ${iface}
      ${ns} ip addr replace ${ipWithPrefix} dev ${iface}
      ${lib.optionalString (uplink.subnet != null) ''
        ${ns} ip route replace ${uplink.subnet} dev ${iface}
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
      description = "Configure inet netns veth pair for host<->inet connectivity";
      after = [ "network-pre.target" ];
      wants = [ "network-pre.target" ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = pkgs.writeShellScript "melinoe-inet-setup" ''
          ip netns add inet 2>/dev/null || true
          ${ns} ip link set lo up
          ip link del ${netCfg.uplinkVeth} 2>/dev/null || true
          ip link add ${netCfg.uplinkVeth} type veth peer name main
          ip link set main netns inet
          ip addr replace ${hostAddr}/32 dev ${netCfg.uplinkVeth}
          ip link set ${netCfg.uplinkVeth} up
          ${ns} ip link set main up
          ${ns} ip addr replace ${uplinkAddr}/32 dev main
          ${ns} ip route replace ${hostAddr}/32 dev main
          ip route replace ${uplinkAddr} dev ${netCfg.uplinkVeth}
          ip route replace default via ${uplinkAddr} dev ${netCfg.uplinkVeth}
          ip rule del fwmark ${toString netCfg.uplinkFwMark} lookup ${toString netCfg.uplinkFwMark} >/dev/null 2>&1 || true
          ip rule add fwmark ${toString netCfg.uplinkFwMark} lookup ${toString netCfg.uplinkFwMark}
          ip route replace default via ${uplinkAddr} dev ${netCfg.uplinkVeth} table ${toString netCfg.uplinkFwMark}
          ${ns} sysctl -w net.ipv4.conf.default.rp_filter=0
          ${ns} sysctl -w net.ipv4.conf.all.rp_filter=0
          ${lib.concatStringsSep "\n" (lib.imap0 mkUplinkScript netCfg.uplinks)}
          ${ns} nft delete table ip nat 2>/dev/null || true
          ${ns} nft -f ${inetNftRuleset}
          ${
            let
              firstUplink = lib.head netCfg.uplinks;
            in
            lib.optionalString (firstUplink.gateway != null) ''
              ${ns} ip route replace default via ${firstUplink.gateway} dev ${uplinkIface 0 firstUplink}
            ''
          }
        '';
      };
      path = [
        pkgs.iproute2
        pkgs.nftables
        pkgs.procps
      ];
    };
  };
}
