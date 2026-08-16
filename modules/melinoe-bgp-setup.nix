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
  # Start of the 16-bit private ASN range (RFC 6996). Node N's AS is this + N.
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
    neighbor ${peer.addr} timers 1 3
  '';
  mkNeighborAfi = peer: ''
    neighbor ${peer.addr} activate
    neighbor ${peer.addr} route-map NODE-IN in
    neighbor ${peer.addr} route-map NODE-OUT out
  '';
in
{
  config = {
    melinoe.node.networking.specialWgAccess.tcp = lib.mkIf netCfg.enabled [ 179 ];

    services.frr = lib.mkIf netCfg.enabled {
      bgpd.enable = true;
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
  };
}
