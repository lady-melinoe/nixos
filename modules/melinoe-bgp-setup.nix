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

  # Temporary BFD test: only nodes 6 and 7 participate.
  bfdEnabled = nodeID == 6 || nodeID == 7;

  bfdPeer =
    if nodeID == 6 then {
      addr = melinoeNodeWgIP 7;
      localAddr = localWgAddr;
    } else if nodeID == 7 then {
      addr = melinoeNodeWgIP 6;
      localAddr = localWgAddr;
    } else
      null;

  # Multihop BFD deliberately does not specify an interface.
  # This lets us test whether FRR follows normal routing to the peer.
  bfdConfig = lib.optionalString bfdEnabled ''
    bfd
      peer ${bfdPeer.addr} multihop local-address ${bfdPeer.localAddr}
        transmit-interval 300
        receive-interval 300
        detect-multiplier 3
      exit
    exit
  '';

  mkNeighborStanza = peer: ''
    neighbor ${peer.addr} remote-as ${toString (bgpAsBase + peer.id)}
    neighbor ${peer.addr} update-source ${localWgAddr}
    neighbor ${peer.addr} timers 1 3
    ${lib.optionalString (
      bfdEnabled
      && (
        (nodeID == 6 && peer.id == 7)
        || (nodeID == 7 && peer.id == 6)
      )
    ) "neighbor ${peer.addr} bfd"}
  '';

  mkNeighborAfi = peer: ''
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
      bfdd.enable = bfdEnabled;

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

          ${bfdConfig}

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
  };
}
