{
  config,
  pkgs,
  melinoeLib,
  ...
}:
{
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
    # These test files start a server and sleep a fixed 5s instead of polling
    # for readiness, so they fail on slow or busy builders. Drop once upstream fixes it.
    package = pkgs.glances.overridePythonAttrs (old: {
      disabledTestPaths = (old.disabledTestPaths or [ ]) ++ [
        "tests/test_restful.py"
        "tests/test_browser_restful.py"
        "tests/test_mcp.py"
        "tests/test_xmlrpc.py"
      ];
    });
    port = 61208;
    extraArgs = [
      "--webserver"
      "-B"
      config.melinoe.node.address
    ];
  };
  systemd.services.glances = melinoeLib.afterMeshAddress;
}
