{ config, lib, pkgs, ... }:
let
  cfg = config.melinoe;
  firstUplink = lib.head cfg.internet;
  hostAddr = "198.18.0.${toString cfg.nodeId}";
  mkUplinkScript = idx: uplink:
    let
      ifaces = uplink.iface;
      isBonded = builtins.length ifaces > 1;
      iface = if isBonded then "bond${toString idx}" else builtins.head ifaces;
      ipParts = lib.splitString "/" uplink.ip;
      ipAddr = builtins.head ipParts;
      ipPrefixFromIp = if builtins.length ipParts > 1 then builtins.elemAt ipParts 1 else null;

      subnetParts = if uplink.subnet != null then lib.splitString "/" uplink.subnet else null;
      subnetPrefix = if subnetParts != null && builtins.length subnetParts > 1 then builtins.elemAt subnetParts 1 else null;

      ipWithPrefix =
        if subnetPrefix != null then "${ipAddr}/${subnetPrefix}"
        else if ipPrefixFromIp != null then uplink.ip
        else "${ipAddr}/32";
    in
    ''
      ${lib.concatMapStringsSep "\n" (name: ''
        ip link set ${name} down
        ip link set ${name} netns inet
      '') ifaces}
      ${lib.optionalString (!isBonded) ''
        ip netns exec inet ip link set ${iface} up
      ''}
      ${lib.optionalString isBonded ''
        ip netns exec inet ip link add ${iface} type bond mode 802.3ad
        ${lib.optionalString (uplink.lacpRate != null) ''
          ip netns exec inet ip link set ${iface} type bond lacp_rate ${uplink.lacpRate}
        ''}
        ip netns exec inet ip link set ${iface} up
        ${lib.concatMapStringsSep "\n" (name: ''
          ip netns exec inet ip link set ${name} master ${iface}
        '') ifaces}
      ''}
      ip netns exec inet ip addr flush dev ${iface}
      ip netns exec inet ip addr replace ${ipWithPrefix} dev ${iface}
      ${lib.optionalString (uplink.subnet != null) ''
        ip netns exec inet ip route replace ${uplink.subnet} dev ${iface}
      ''}
      ip netns exec inet iptables -t nat -C POSTROUTING -o ${iface} -j MASQUERADE
      ip netns exec inet iptables -t nat -A POSTROUTING -o ${iface} -j MASQUERADE
      ip netns exec inet iptables -t nat -C PREROUTING -d ${ipAddr} -j DNAT --to-destination ${hostAddr}
      ip netns exec inet iptables -t nat -C PREROUTING -i ${iface} -d ${ipAddr} -j DNAT --to-destination ${hostAddr}
      ip netns exec inet iptables -t nat -A PREROUTING -i ${iface} -d ${ipAddr} -j DNAT --to-destination ${hostAddr}
    '';
in {
  systemd.services.melinoe-inet-setup = lib.mkIf (cfg.internet != [ ]) {
    description = "Configure inet netns veth pair for host<->inet connectivity";
    after = [ "network-pre.target" ];
    wants = [ "network-pre.target" ];
    wantedBy = [ "multi-user.target" ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
      ExecStart = pkgs.writeShellScript "melinoe-inet-setup" ''
        ip netns add inet
        ip netns exec inet ip link set lo up
        ip link del inet0
        ip link add inet0 type veth peer name main
        ip link set main netns inet
        ip addr replace ${hostAddr}/32 dev inet0
        ip link set inet0 up
        ip netns exec inet ip link set main up
        ip netns exec inet ip addr replace 198.18.0.255/32 dev main
        ip netns exec inet ip route replace ${hostAddr}/32 dev main
        ip route add 198.18.0.255 dev inet0
        ip route add default via 198.18.0.255 dev inet0
        ip rule del fwmark 51820 lookup 51820 >/dev/null 2>&1 || true
        ip rule add fwmark 51820 lookup 51820
        ip route replace default via 198.18.0.255 dev inet0 table 51820
        ip netns exec inet sysctl -w net.ipv4.conf.default.rp_filter=0
        ip netns exec inet sysctl -w net.ipv4.conf.all.rp_filter=0

        ${lib.concatStringsSep "\n" (lib.imap0 mkUplinkScript cfg.internet)}

        # this one is only for the first interface for when we add multi iface support
        ${lib.optionalString (firstUplink.gateway != null) ''
          ip netns exec inet ip route add default via ${firstUplink.gateway} dev ${if builtins.length firstUplink.iface > 1 then "bond0" else builtins.head firstUplink.iface}
        ''}
      '';
    };
    path = [ pkgs.iproute2 pkgs.iptables pkgs.procps ];
  };
}
