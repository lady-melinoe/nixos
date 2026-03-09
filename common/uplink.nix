{ config, lib, pkgs, ... }:

let
  cfg = config.melinoe;

  firstUplink = lib.head cfg.internet;

  hostAddr = "198.18.0.${toString cfg.nodeId}";

  mkUplinkScript = uplink:
    let
      iface = builtins.head uplink.iface;
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
      ip link set ${iface} down
      ip link set ${iface} netns inet
      ip netns exec inet ip link set ${iface} up
      ip netns exec inet ip addr flush dev ${iface}
      ip netns exec inet ip addr replace ${ipWithPrefix} dev ${iface}
      ${lib.optionalString (uplink.subnet != null) ''
        ip netns exec inet ip route replace ${uplink.subnet} dev ${iface}
      ''}
      ip netns exec inet iptables -t nat -C POSTROUTING -o ${iface} -j MASQUERADE
      ip netns exec inet iptables -t nat -A POSTROUTING -o ${iface} -j MASQUERADE
      ip netns exec inet iptables -t nat -C PREROUTING -d ${ipAddr} -j DNAT --to-destination ${hostAddr}
      ip netns exec inet iptables -t nat -A PREROUTING -d ${ipAddr} -j DNAT --to-destination ${hostAddr}
    '';
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
          ip netns add inet
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
          ip netns exec inet sysctl -w net.ipv4.conf.default.rp_filter=0
          ip netns exec inet sysctl -w net.ipv4.conf.all.rp_filter=0          

          ${lib.concatStringsSep "\n" (map mkUplinkScript cfg.internet)}

          # this one is only for the first interface for when we add multi iface support
          ${lib.optionalString (firstUplink.gateway != null) ''
            ip netns exec inet ip route add default via ${firstUplink.gateway} dev ${builtins.head firstUplink.iface}
          ''}



        '';
      };
      path = [ pkgs.iproute2 pkgs.iptables pkgs.procps ];
    };
  };
}
