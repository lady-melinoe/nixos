{ config, lib, pkgs, ... }:
let
  cfg = config.melinoe;
  keys = lib.filterAttrs (_: v: v != null) (lib.mapAttrs (_: node: node.wgPubkey) cfg.publicNodes);
  basePort = 64512;
  wgPrefix = "198.19.3";
  nodeID = cfg.nodeId;
  peers = map (p: { id = p.id; addr = "${wgPrefix}.${toString p.id}"; }) cfg.peers;
  localWgAddr = "${wgPrefix}.${toString nodeID}";
  peerToCfg = peer: let
    pid = peer.id;
    allowed = lib.unique (peer.allowedIPs ++ [ "${wgPrefix}.${toString pid}/32" ]);
  in {
    publicKey = keys."${toString pid}";
    allowedIPs = allowed;
    endpoint = "${peer.endpoint}:${toString (basePort + cfg.nodeId)}";
    persistentKeepalive = peer.persistentKeepalive;
  };
  runtimeShell = "${pkgs.runtimeShell}";
  ipBin = "${pkgs.iproute2}/bin/ip";
  mkInterface = peer:
    let
      ifName = "wg-${toString peer.id}";
    in {
      name = ifName;
      value = {
        address = [ ];
        listenPort = basePort + peer.id;
        privateKeyFile = "/etc/melinoe/wg.privatekey";
        extraOptions = {
          FwMark = 51820;
        };
        peers = [ (peerToCfg peer) ];
        postUp = ''
          ${runtimeShell} -c '${ipBin} route del ${wgPrefix}.${toString peer.id}/32 dev ${ifName} || true'
          ${runtimeShell} -c '${ipBin} address replace ${localWgAddr}/32 peer ${wgPrefix}.${toString peer.id}/32 dev ${ifName}'
          ${runtimeShell} -c '${ipBin} route del 198.19.3.0/24 dev ${ifName} || true'
          ${runtimeShell} -c '${ipBin} route del 198.51.100.0/24 dev ${ifName} || true'
        '';
      };
    };
  interfaces = lib.listToAttrs (map mkInterface cfg.peers);
  ports = map (peer: basePort + peer.id) cfg.peers;
  wgWatchdogScript = pkgs.writeShellScript "melinoe-wg-watchdog" ''
    #!/usr/bin/env bash
    set -euo pipefail

    for IFACE in ${lib.concatStringsSep " " (map (peer: "wg-${toString peer.id}") cfg.peers)}; do
      UNIT="wg-quick-''${IFACE}.service"

      if systemctl is-active --quiet "$UNIT" && ip link show "$IFACE" >/dev/null 2>&1; then
        continue
      fi

      systemctl restart "$UNIT"
    done
  '';
in {
  networking.wg-quick.interfaces = lib.mkIf (cfg.peers != [ ]) interfaces;
  melinoe.wgPorts = lib.mkIf (cfg.peers != [ ]) ports;

  systemd.services.melinoe-wg-watchdog = lib.mkIf (cfg.peers != [ ]) {
    description = "Restart WireGuard interfaces that are down";
    after = [ "network-online.target" ];
    wants = [ "network-online.target" ];
    path = [ pkgs.iproute2 pkgs.systemd pkgs.gnugrep ];
    serviceConfig = {
      Type = "oneshot";
      ExecStart = wgWatchdogScript;
    };
  };

  systemd.timers.melinoe-wg-watchdog = lib.mkIf (cfg.peers != [ ]) {
    wantedBy = [ "timers.target" ];
    timerConfig = {
      OnBootSec = "2min";
      OnUnitActiveSec = "2min";
      AccuracySec = "30s";
      Persistent = true;
    };
  };
}
