{ config, ... }:
{
  melinoe.haproxy = {
    backends = {
      nginx1 = config.melinoe.cluster.vms.npm.address;
      nginx2 = config.melinoe.cluster.vms.npmalt.address;
    };
    proxyProtocolSources = [ "192.168.11.2" ];
  };
}
