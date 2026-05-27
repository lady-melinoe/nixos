{ config, lib, pkgs, ... }:

let
  cfg = config.melinoe;

  ns = "ip netns exec inet";
  hostAddr = "198.18.0.${toString cfg.nodeId}";

  uplinkIface = idx: uplink:
    if builtins.length uplink.iface > 1
    then "bond${toString idx}"
    else builtins.head uplink.iface;

  uplinkIpAddr = uplink:
    builtins.head (lib.splitString "/" uplink.ip);

  nftRuleset = pkgs.writeText "melinoe-inet.nft" ''
    table ip nat {
      chain prerouting {
        type nat hook prerouting priority dstnat; policy accept;
        ${lib.concatStringsSep "\n" (lib.imap0 (idx: uplink:
          ''iifname "${uplinkIface idx uplink}" ip daddr ${uplinkIpAddr uplink} dnat to ${hostAddr}''
        ) cfg.internet)}
      }

      chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        ${lib.concatStringsSep "\n" (lib.imap0 (idx: uplink:
          ''oifname "${uplinkIface idx uplink}" masquerade''
        ) cfg.internet)}
      }
    }
  '';

  mkUplinkScript = idx: uplink:
    let
      ifaces = uplink.iface;
      isBonded = builtins.length ifaces > 1;
      iface = uplinkIface idx uplink;

      ipParts = lib.splitString "/" uplink.ip;
      ipAddr = builtins.head ipParts;
      ipPrefixFromIp =
        if builtins.length ipParts > 1
        then builtins.elemAt ipParts 1
        else null;

      subnetParts =
        if uplink.subnet != null
        then lib.splitString "/" uplink.subnet
        else null;

      subnetPrefix =
        if subnetParts != null && builtins.length subnetParts > 1
        then builtins.elemAt subnetParts 1
        else null;

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
        ip netns add inet 2>/dev/null || true
        ${ns} ip link set lo up

        ip link del inet0 2>/dev/null || true
        ip link add inet0 type veth peer name main
        ip link set main netns inet

        ip addr replace ${hostAddr}/32 dev inet0
        ip link set inet0 up

        ${ns} ip link set main up
        ${ns} ip addr replace 198.18.0.255/32 dev main
        ${ns} ip route replace ${hostAddr}/32 dev main

        ip route replace 198.18.0.255 dev inet0
        ip route replace default via 198.18.0.255 dev inet0

        ip rule del fwmark 51820 lookup 51820 >/dev/null 2>&1 || true
        ip rule add fwmark 51820 lookup 51820
        ip route replace default via 198.18.0.255 dev inet0 table 51820

        ${ns} sysctl -w net.ipv4.conf.default.rp_filter=0
        ${ns} sysctl -w net.ipv4.conf.all.rp_filter=0

        ${lib.concatStringsSep "\n" (lib.imap0 mkUplinkScript cfg.internet)}

        ${ns} nft delete table ip nat 2>/dev/null || true
        ${ns} nft -f ${nftRuleset}

        ${let
          firstUplink = lib.head cfg.internet;
        in lib.optionalString (firstUplink.gateway != null) ''
          ${ns} ip route replace default via ${firstUplink.gateway} dev ${uplinkIface 0 firstUplink}
        ''}
      '';
    };

    path = [
      pkgs.iproute2
      pkgs.nftables
      pkgs.procps
    ];
  };
}
