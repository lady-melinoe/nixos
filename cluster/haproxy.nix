{ config, ... }:
{
  melinoe.haproxy = {
    backends = {
      nginx1 = {
        address = config.melinoe.cluster.vms.npm.address;
      };
      nginx2 = {
        address = config.melinoe.cluster.vms.npmalt.address;
      };
    };
    proxyProtocolSources = [ "192.168.11.2" ];
  };
}
