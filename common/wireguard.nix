{ config, lib, ... }:

let
  cfg = config.melinoe;
  keys = import ./wg-keys.nix;
  basePort = 64512;
  wgPrefix = "198.19.3";
  localWgAddr = "${wgPrefix}.${toString cfg.nodeId}";

  peerToCfg = peer: let
    pid = peer.id;
    allowed = lib.unique (peer.allowedIPs ++ [ "${wgPrefix}.0/24" "${wgPrefix}.${toString pid}/32" ]);
  in {
    publicKey = keys."${toString pid}";
    allowedIPs = allowed;
    endpoint = "${peer.endpoint}:${toString (basePort + cfg.nodeId)}";
    persistentKeepalive = peer.persistentKeepalive;
  };
  extraBgpPeers = map (p: { id = p.id; addr = "${wgPrefix}.${toString p.id}"; }) cfg.wgPeers;

  mkInterface = peer: {
    name = "wg-${toString peer.id}";
    value = {
      ips = [ "${localWgAddr}/32" ];
      listenPort = basePort + peer.id;
      privateKeyFile = "/etc/melinoe/wg.privatekey";
      peers = [ (peerToCfg peer) ];
    };
  };

  interfaces = lib.listToAttrs (map mkInterface cfg.wgPeers);
  ports = map (peer: basePort + peer.id) cfg.wgPeers;
in {
  config = lib.mkIf (cfg.wgPeers != [ ]) {
    networking.wireguard.interfaces = interfaces;

    # Open just the ports we actually listen on.
    melinoe.wgPorts = ports;

    # Auto-add BGP peers on the WG subnet.
    melinoe.bgpPeers = lib.mkAfter extraBgpPeers;

    # Ensure the local WG source address exists (once, on loopback).
  };
}
