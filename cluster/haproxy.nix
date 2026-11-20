{ config, ... }:
{
  melinoe.haproxy = {
    backends = {
      nginx1 = {
        address = config.melinoe.cluster.vms.npm.address;
        checkHost = "nginx.melinoe.xyz";
      };
      nginx2 = {
        address = config.melinoe.cluster.vms.npmalt.address;
        checkHost = "nginx-alt.melinoe.xyz";
      };
    };
    proxyProtocolSources = [ "192.168.11.2" ];
  };
}
