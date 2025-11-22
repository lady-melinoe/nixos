{ config, pkgs, ... }:
let
  nodeID = config.melinoe.nodeId;
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
}
