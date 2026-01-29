{ config, lib, ... }:

let
  cfg = config.melinoe;
  keys = import ./wg-keys.nix;
  basePort = 64512;
  hostId = cfg.nodeId;
  localListenPort = basePort + hostId;
  wgPrefix = "198.19.3";
  localWgAddr = "${wgPrefix}.${toString hostId}";

  peerToCfg = peer: let
    pid = peer.id;
    port = basePort + pid;
    allowed = lib.unique (peer.allowedIPs ++ [ "${wgPrefix}.0/24" "${wgPrefix}.${toString pid}/32" ]);
  in {
    publicKey = keys."${toString pid}";
    allowedIPs = allowed;
    endpoint = peer.endpoint;
    persistentKeepalive = peer.persistentKeepalive;
  };
in {
  config = lib.mkIf (cfg.wgPeers != [ ]) {
    networking.wireguard.interfaces."wg-mesh" = {
      ips = [ "${localWgAddr}/32" ];
      listenPort = localListenPort;
      privateKeyFile = "/etc/melinoe/wg-${config.networking.hostName}.key";
      peers = map peerToCfg cfg.wgPeers;
    };

    # Open just the ports we actually listen on.
    melinoe.wgPorts = [ localListenPort ];

    # Auto-add BGP peers on the WG subnet.
    melinoe.bgpPeers = cfg.bgpPeers ++ (map (p: { id = p.id; addr = "198.19.3.${toString p.id}"; }) cfg.wgPeers);
  };
}
