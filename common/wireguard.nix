{ config, lib, ... }:

let
  cfg = config.melinoe;
  keys = import ./wg-keys.nix;
  basePort = 64512;
  wgPrefix = "198.19.3";
  localWgAddr = "${wgPrefix}.${toString cfg.nodeId}";

  peerToCfg = peer: let
    pid = peer.id;
    allowed = lib.unique (peer.allowedIPs ++ [ "${wgPrefix}.${toString pid}/32" ]);
  in {
    publicKey = keys."${toString pid}";
    allowedIPs = allowed;
    endpoint = "${peer.endpoint}:${toString (basePort + cfg.nodeId)}";
    persistentKeepalive = peer.persistentKeepalive;
  };

  mkInterface = peer:
    let
      ifName = "wg-${toString peer.id}";
    in {
      name = ifName;
      value = {
        ips = [ ];
        listenPort = basePort + peer.id;
        privateKeyFile = "/etc/melinoe/wg.privatekey";
        peers = [ (peerToCfg peer) ];
        postSetup = ''
          ip address replace ${localWgAddr}/32 peer ${wgPrefix}.${toString peer.id}/32 dev ${ifName}
          ${pkgs.runtimeShell} -c 'ip route del 198.19.3.0/24 dev ${ifName} || true'
          ${pkgs.runtimeShell} -c 'ip route del 198.51.100.0/24 dev ${ifName} || true'
        '';
      };
    };

  interfaces = lib.listToAttrs (map mkInterface cfg.wgPeers);
  ports = map (peer: basePort + peer.id) cfg.wgPeers;
in {
  config = lib.mkIf (cfg.wgPeers != [ ]) {
    networking.wireguard.interfaces = interfaces;
    melinoe.wgPorts = ports;
  };
}
