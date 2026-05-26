{ config, lib, ... }:
let
  cfg = config.melinoe;
  nodeID = cfg.nodeId;
  bgpAs = 64512 + nodeID;
  wgPrefix = "198.19.3";
  peers = map (p: { id = p.id; addr = "${wgPrefix}.${toString p.id}"; }) cfg.peers;
  localWgAddr = "${wgPrefix}.${toString nodeID}";
  mkNeighborStanza = peer: ''
    neighbor ${peer.addr} remote-as ${toString (64512 + peer.id)}
    neighbor ${peer.addr} update-source ${localWgAddr}
    neighbor ${peer.addr} timers 1 3
  '';
  mkNeighborAfi = peer: ''
    neighbor ${peer.addr} activate
    neighbor ${peer.addr} route-map NODE-IN in
    neighbor ${peer.addr} route-map NODE-OUT out
  '';
in {
  services.frr = {
    bgpd.enable = true;
    config =
      let
        neighborLines = lib.concatMapStrings mkNeighborStanza peers;
        neighborAfiLines = lib.concatMapStrings mkNeighborAfi peers;
      in ''

        ip prefix-list NODE-LOOPS permit 198.51.100.0/24 le 32

        route-map NODE-IN permit 10
         match ip address prefix-list NODE-LOOPS

        route-map NODE-OUT permit 10
         match ip address prefix-list NODE-LOOPS

        router bgp ${toString bgpAs}
          bgp router-id 198.51.100.${toString nodeID}
          bgp fast-convergence
${neighborLines}

          address-family ipv4 unicast
            network 198.51.100.${toString nodeID}/32
${neighborAfiLines}
          exit-address-family
        !
      '';
  };
}
