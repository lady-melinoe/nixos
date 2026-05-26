{ config, lib, ... }:
let
  cfg = config.melinoe;
  nodeID = cfg.nodeId;
  bgpAs = 64512 + nodeID;
  wgPrefix = "198.19.3";
  peers = map (p: { id = p.id; addr = "${wgPrefix}.${toString p.id}"; }) cfg.peers;
  localWgAddr = "${wgPrefix}.${toString nodeID}";
  mkNeighborStanza = peer: ''
    neighbor ${peer.addr} interface wg-${toString peer.id} remote-as ${toString (64512 + peer.id)}
    neighbor ${peer.addr} update-source ${localWgAddr}
    neighbor ${peer.addr} bfd
  '';
  mkNeighborAfi = peer: ''
    neighbor ${peer.addr} activate
    neighbor ${peer.addr} route-map NODE-IN in
    neighbor ${peer.addr} route-map NODE-OUT out
  '';
  mkBfdStanza = peer: ''
    peer ${peer.addr} interface wg-${toString peer.id}
      transmit-interval 1000
      receive-interval 1000
      detect-multiplier 3
      no shutdown
    !
  '';
in {
  services.frr = {
    bgpd.enable = true;
    bfdd.enable = true;
    config =
      let
        neighborLines = lib.concatMapStrings mkNeighborStanza peers;
        neighborAfiLines = lib.concatMapStrings mkNeighborAfi peers;
        bfdLines = lib.concatMapStrings mkBfdStanza peers;
      in ''

        ip prefix-list NODE-LOOPS permit 198.51.100.0/24 le 32

        route-map NODE-IN permit 10
         match ip address prefix-list NODE-LOOPS

        route-map NODE-OUT permit 10
         match ip address prefix-list NODE-LOOPS

        router bgp ${toString bgpAs}
          bgp router-id 198.51.100.${toString nodeID}
${neighborLines}

          address-family ipv4 unicast
            network 198.51.100.${toString nodeID}/32
${neighborAfiLines}
          exit-address-family
        !
      '';
  };
}
