{ config, melinoeLib, ... }:
{
  services.qemuGuest.enable = true;
  systemd.services.qemu-guest-agent.unitConfig.ConditionVirtualization = "vm";

  services.chrony = {
    enable = true;
    servers = map (n: "${toString n}.au.pool.ntp.org") [
      0
      1
      2
      3
    ];
  };

  services.iperf3.enable = true;
  melinoe.firewall.meshPorts = {
    tcp = [ 5201 ];
    udp = [ 5201 ];
  };

  services.glances = {
    enable = true;
    port = 61208;
    extraArgs = [
      "--webserver"
      "-B"
      config.melinoe.node.address
    ];
  };
  systemd.services.glances = melinoeLib.afterMeshAddress;
}
