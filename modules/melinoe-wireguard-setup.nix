{
  config,
  lib,
  pkgs,
  melinoeNodeWgIP,
  ...
}:
let
  cfg = config.melinoe;
  netCfg = config.melinoe.node.networking;
  nodeID = cfg.node.id;
  basePort = netCfg.wireguardBasePort;
  localWgAddr = melinoeNodeWgIP nodeID;
  keys = lib.filterAttrs (_: v: v != null) (lib.mapAttrs (_: node: node.wgPubkey) cfg.nodePublicInfo);
  peerToCfg =
    peer:
    let
      pid = peer.id;
      pidStr = toString pid;
      allowed = lib.unique (peer.allowedIPs ++ [ "${melinoeNodeWgIP pid}/32" ]);
      resolvedEndpoint =
        if peer.endpoint != null then peer.endpoint else cfg.nodePublicInfo.${pidStr}.defaultEndpoint;
    in
    {
      name = "wg-${pidStr}-peerconf";
      publicKey = keys."${pidStr}";
      allowedIPs = allowed;
      endpoint = "${resolvedEndpoint}:${toString (basePort + cfg.node.id)}";
      persistentKeepalive = peer.persistentKeepalive;
    };
  runtimeShell = "${pkgs.runtimeShell}";
  ipBin = "${pkgs.iproute2}/bin/ip";
  mkInterface =
    peer:
    let
      ifName = "wg-${toString peer.id}";
    in
    {
      name = ifName;
      value = {
        listenPort = basePort + peer.id;
        privateKeyFile = "/etc/melinoe/wg.privatekey";
        allowedIPsAsRoutes = false;
        fwMark = toString netCfg.uplinkFwMark;
        dynamicEndpointRefreshSeconds = 15;
        peers = [ (peerToCfg peer) ];
        postSetup = ''
          ${runtimeShell} -c '${ipBin} address replace ${localWgAddr}/32 peer ${melinoeNodeWgIP peer.id}/32 dev ${ifName}'
        '';
      };
    };
  interfaces = lib.listToAttrs (map mkInterface netCfg.peers);
  ports = map (peer: basePort + peer.id) netCfg.peers;
in
{
  config = lib.mkIf netCfg.enabled {
    networking.wireguard.interfaces = lib.mkIf (netCfg.peers != [ ]) interfaces;
    melinoe.node.networking.openPorts.udp = lib.mkIf (netCfg.peers != [ ]) ports;
    systemd.services = lib.mkIf (netCfg.peers != [ ]) (
      lib.listToAttrs (
        map (peer: {
          name = "wireguard-wg-${toString peer.id}";
          value = {
            after = [ "melinoe-inet-setup.service" ];
            wants = [ "melinoe-inet-setup.service" ];
            stopIfChanged = false;
            restartIfChanged = false;
            serviceConfig = {
              Restart = lib.mkForce "on-failure";
              RestartSec = 5;
            };
            startLimitIntervalSec = 0;
          };
        }) netCfg.peers
      )
    );
  };
}
