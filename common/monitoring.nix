{ config, ... }:

{
  services.iperf3.enable = true;
  services.glances.enable = true;
  services.glances.port = 61208;
  services.glances.extraArgs = [ "--webserver" "-B" "198.18.0.${toString config.melinoe.nodeId}" ];
}
