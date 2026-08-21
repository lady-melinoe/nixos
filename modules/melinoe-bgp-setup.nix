{
  config,
  lib,
  melinoeNodeWgIP,
  melinoeNodeLoopbackIP,
  ...
}:
let
  cfg = config.melinoe;
  netCfg = config.melinoe.node.networking;
  addr = config.melinoe.cluster.networking;

  nodeID = cfg.node.id;

  bgpAsBase = 64512; # start of private ASN range (RFC 6996)
  bgpAs = bgpAsBase + nodeID;

  localWgAddr = melinoeNodeWgIP nodeID;
  localLoopbackAddr = melinoeNodeLoopbackIP nodeID;

  peers = map (p: {
    id = p.id;
    addr = melinoeNodeWgIP p.id;
  }) netCfg.peers;

  # Temporary experiment:
  # nodes 6 and 7 use interface-based BGP over their direct WG link.
  interfaceBGP = nodeID == 6 || nodeID == 7;

  isTargetPeer = peer:
    interfaceBGP
    && (
      (nodeID == 6 && peer.id == 7)
      || (nodeID == 7 && peer.id == 6)
    );

  mkNeighborStanza = peer:
    if isTargetPeer peer then
      ''
        neighbor wg-${toString peer.id} interface remote-as ${toString (bgpAsBase + peer.id)}
        neighbor wg-${toString peer.id} bfd
      ''
    else
      ''
        neighbor ${peer.addr} remote-as ${toString (bgpAsBase + peer.id)}
        neighbor ${peer.addr} update-source ${localWgAddr}
        neighbor ${peer.addr} timers 1 3
      '';

  mkNeighborAfi = peer:
    if isTargetPeer peer then
      ''
        neighbor wg-${toString peer.id} activate
        neighbor wg-${toString peer.id} route-map NODE-IN in
        neighbor wg-${toString peer.id} route-map NODE-OUT out
      ''
    else
      ''
        neighbor ${peer.addr} activate
        neighbor ${peer.addr} route-map NODE-IN in
        neighbor ${peer.addr} route-map NODE-OUT out
      '';
in
{
  config = {
    melinoe.node.networking.specialWgAccess.tcp =
      lib.mkIf netCfg.enabled [ 179 ];

    services.frr = lib.mkIf netCfg.enabled {
      bgpd.enable = true;
      bfdd.enable = true;

      config =
        let
          neighborLines = lib.concatMapStrings mkNeighborStanza peers;
          neighborAfiLines = lib.concatMapStrings mkNeighborAfi peers;
        in
        ''
          ip prefix-list NODE-LOOPS permit ${addr.bgpCidr} le 32

          route-map NODE-IN permit 10
           match ip address prefix-list NODE-LOOPS

          route-map NODE-OUT permit 10
           match ip address prefix-list NODE-LOOPS

          router bgp ${toString bgpAs}
            bgp router-id ${localLoopbackAddr}
            bgp fast-convergence
          ${neighborLines}
            address-family ipv4 unicast
              network ${localLoopbackAddr}/32
          ${neighborAfiLines}
            exit-address-family
          !
        '';
    };

    # FRR currently has an upstream reload problem for us.
    # Force NixOS to stop and start FRR when its definition changes,
    # rather than attempting a systemd reload.
    systemd.services.frr = lib.mkIf netCfg.enabled {
      reloadIfChanged = lib.mkForce false;
      restartIfChanged = lib.mkForce true;
      stopIfChanged = lib.mkForce true;
    };
  };
}
