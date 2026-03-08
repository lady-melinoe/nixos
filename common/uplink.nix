{ config, lib, pkgs, ... }:

let
  cfg = config.melinoe;

  uplink = lib.head cfg.internet;

  ipParts = lib.splitString "/" uplink.ip;
  ipAddr = builtins.head ipParts;
  ipPrefixFromIp = if builtins.length ipParts > 1 then builtins.elemAt ipParts 1 else null;

  subnetParts = if uplink.subnet != null then lib.splitString "/" uplink.subnet else null;
  subnetPrefix = if subnetParts != null && builtins.length subnetParts > 1 then builtins.elemAt subnetParts 1 else null;

  ipWithPrefix =
    if subnetPrefix != null then "${ipAddr}/${subnetPrefix}"
    else if ipPrefixFromIp != null then uplink.ip
    else "${ipAddr}/32";

  hostAddr = "198.18.0.${toString cfg.nodeId}";
in
{
  config = lib.mkIf (cfg.internet != [ ]) {
    systemd.services.melinoe-inet-setup = {
      description = "Configure inet netns veth pair for host<->inet connectivity";
      after = [ "network-pre.target" ];
      wants = [ "network-pre.target" ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = pkgs.writeShellScript "melinoe-inet-setup" ''
          ip netns add inet 2>/dev/null
          ip link del inet0 2>/dev/null

          ip link add inet0 type veth peer name main
          ip link set main netns inet

          ip addr replace ${hostAddr}/32 dev inet0
          ip link set inet0 up

          ip netns exec inet ip addr replace 198.18.0.255/32 dev main
          ip netns exec inet ip link set main up

          ip netns exec inet ip route replace ${hostAddr}/32 dev main

          ip link set ${uplink.iface} down 2>/dev/null
          ip link set ${uplink.iface} netns inet

          ip netns exec inet ip link set ${uplink.iface} up
          ip netns exec inet ip addr flush dev ${uplink.iface}
          ip netns exec inet ip addr replace ${ipWithPrefix} dev ${uplink.iface}

          ip netns exec inet iptables -t nat -C POSTROUTING -o ${uplink.iface} -j MASQUERADE
          ip netns exec inet iptables -t nat -A POSTROUTING -o ${uplink.iface} -j MASQUERADE

          ip route add 198.18.0.255 dev inet0
          ip route add default via 198.18.0.255 dev inet0

          ${lib.optionalString (uplink.gateway != null) ''
            ip netns exec inet ip route add default via ${uplink.gateway} dev ${uplink.iface}
          ''}

          ip netns exec inet iptables -t nat -C PREROUTING -d ${ipAddr} -j DNAT --to-destination ${hostAddr}
          ip netns exec inet iptables -t nat -A PREROUTING -d ${ipAddr} -j DNAT --to-destination ${hostAddr}
          ip netns exec inet sysctl -w net.ipv4.conf.default.rp_filter=0
          ip netns exec inet sysctl -w net.ipv4.conf.all.rp_filter=0
        '';
      };
      path = [ pkgs.iproute2 pkgs.iptables pkgs.procps ];
    };
  };
}
