{
  config,
  lib,
  melinoeNodeWgIP,
  melinoeNodeLoopbackIP,
  ...
}:

let
  cfg = config.melinoe;
  netCfg = cfg.node.networking;
  addr = cfg.cluster.networking;

  nodeID = cfg.node.id;

  bgpAsBase = 64512;
  bgpAs = bgpAsBase + nodeID;

  localWgAddr = melinoeNodeWgIP nodeID;
  localLoopbackAddr = melinoeNodeLoopbackIP nodeID;

  peers = map (p: {
    id = p.id;
    addr = melinoeNodeWgIP p.id;
  }) netCfg.peers;

  mkNeighborStanza = peer: ''
    neighbor ${peer.addr} remote-as ${toString (bgpAsBase + peer.id)}
    neighbor ${peer.addr} update-source ${localWgAddr}
    neighbor ${peer.addr} ebgp-multihop 2
    neighbor ${peer.addr} bfd
    neighbor ${peer.addr} timers 1 3
  '';

  mkNeighborAfi = peer: ''
    neighbor ${peer.addr} activate
    neighbor ${peer.addr} route-map NODE-IN in
    neighbor ${peer.addr} route-map NODE-OUT out
  '';

  mkBfdPeer = peer: ''
    peer ${peer.addr} local-address ${localWgAddr}
      detect-multiplier 3
      receive-interval 300
      transmit-interval 300
    exit
  '';

  neighborLines =
    lib.concatMapStrings mkNeighborStanza peers;

  neighborAfiLines =
    lib.concatMapStrings mkNeighborAfi peers;

  bfdPeerLines =
    lib.concatMapStrings mkBfdPeer peers;

in
{
  config = {
    # BGP TCP/179 over WireGuard.
    melinoe.node.networking.specialWgAccess.tcp =
      lib.mkIf netCfg.enabled [ 179 ];

    # Singlehop BFD uses UDP/3784.
    melinoe.node.networking.specialWgAccess.udp =
      lib.mkIf netCfg.enabled [ 3784 ];

    # FRR has an upstream reload issue, so force configuration
    # changes to restart the service rather than reload it.
    systemd.services.frr.reloadIfChanged = lib.mkForce false;
    systemd.services.frr.restartIfChanged = lib.mkForce true;

    services.frr = lib.mkIf netCfg.enabled {
      bgpd.enable = true;
      bfdd.enable = true;

      config = ''
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
        exit

        bfd
          ${bfdPeerLines}
        exit
      '';
    };
  };
}
