{ config, lib, pkgs, ... }:
let
  nodeID = config.melinoe.nodeId;
  peers = config.melinoe.bgpPeers;
  bgpAs = 64512 + nodeID;
  mkNeighborStanza = peer: ''
    neighbor ${peer.addr} remote-as ${toString (64512 + peer.id)}
    neighbor ${peer.addr} update-source 198.19.0.${toString nodeID}
  '';
  mkNeighborAfi = peer: ''
    neighbor ${peer.addr} activate
    neighbor ${peer.addr} route-map NODE-IN in
    neighbor ${peer.addr} route-map NODE-OUT out
  '';
in {
  networking.interfaces.lo.ipv4.addresses = [
    {
      address = "198.51.100.${toString nodeID}";
      prefixLength = 32;
    }
    {
      address = "198.18.0.${toString nodeID}";
      prefixLength = 32;
    }
  ];

  systemd.services.melinoe-route-deploy = {
    description = "Run melinoe route deployment";
    after = [ "network-online.target" ];
    wants = [ "network-online.target" ];
    path = [
      pkgs.coreutils
      pkgs.bash
      pkgs.iproute2
      pkgs.frr
      pkgs.jq
      pkgs.curl
      pkgs.gnugrep
      pkgs.gawk
      pkgs.gnused
      pkgs.util-linux
    ];
    serviceConfig = {
      Type = "oneshot";
      ConditionPathIsExecutable = "/etc/nixos/route-deploy.sh";
      ExecStart = "${pkgs.coreutils}/bin/timeout 30 /etc/nixos/route-deploy.sh";
    };
  };

  systemd.timers.melinoe-route-deploy = {
    wantedBy = [ "timers.target" ];
    timerConfig = {
      OnBootSec = "3s";
      OnUnitInactiveSec = "3s";
      AccuracySec = "1s";
    };
  };

  services.frr = {
    bgpd.enable = true;
    bgpd.config =
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
${neighborLines}

          address-family ipv4 unicast
            network 198.51.100.${toString nodeID}/32
${neighborAfiLines}
          exit-address-family
        !
      '';
  };
}
