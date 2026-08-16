{
  config,
  lib,
  melinoeNodeIntraIP,
  ...
}:
let
  mel = config.melinoe;
  cfg = mel.node.networking.ingressPoint.haproxy;
  hostAddr = melinoeNodeIntraIP mel.node.id;

  serverLine =
    port: node:
    "          server ${node.name} ${node.address}:${toString port} send-proxy check check-send-proxy";
  httpServers = lib.concatMapStringsSep "\n" (serverLine cfg.frontendPorts.fe_proxy_http) cfg.backendNodes;
  httpsServers = lib.concatMapStringsSep "\n" (serverLine cfg.frontendPorts.fe_proxy_https) cfg.backendNodes;

  proxyAllowedSrc = lib.concatStringsSep " " cfg.proxyProtocolAllowedSources;

  mkTcpFrontend = name: port: backend: ''
    frontend ${name}
        bind *:${toString port}
        default_backend ${backend}
  '';

  mkTlsFrontend = name: port: backend: ''
    frontend ${name}
        bind *:${toString port}
        tcp-request inspect-delay 5s
        tcp-request content accept if { req_ssl_hello_type 1 }
        default_backend ${backend}
  '';

  mkProxyFrontend =
    {
      name,
      port,
      backend,
      tls,
    }:
    let
      lines =
        [
          "frontend ${name}"
          "    bind *:${toString port}"
          "    tcp-request connection reject if !{ src ${proxyAllowedSrc} }"
          "    tcp-request connection expect-proxy layer4 if { src ${proxyAllowedSrc} }"
        ]
        ++ lib.optional (
          cfg.proxyProtocolAllowedSources != [ ]
        ) "    tcp-request content set-log-level silent if { src ${proxyAllowedSrc} }"
        ++ lib.optionals tls [
          "    tcp-request inspect-delay 5s"
          "    tcp-request content accept if { req_ssl_hello_type 1 }"
        ]
        ++ [ "    default_backend ${backend}" ];
    in
    lib.concatStringsSep "\n" lines + "\n";
in
{
  config = lib.mkIf cfg.enable {
    melinoe.node.networking.openPorts.tcp = cfg.ports;

    services.haproxy = {
      enable = true;
      config = ''
        global
            log /dev/log local0
            log /dev/log local1 notice
            daemon
            maxconn 50000

        defaults
            log     global
            mode    tcp
            option  redispatch
            option  tcplog
            option  dontlognull
            timeout connect 5s
            timeout client  1m
            timeout server  1m
            default-server inter 3s fall 3 rise 2

        ${mkTcpFrontend "fe_http" cfg.frontendPorts.fe_http "be_http"}
        ${mkTlsFrontend "fe_https" cfg.frontendPorts.fe_https "be_https"}
        ${mkTcpFrontend "fe_http_prxbp" cfg.frontendPorts.fe_http_prxbp "be_http"}
        ${mkTlsFrontend "fe_https_prxbp" cfg.frontendPorts.fe_https_prxbp "be_https"}
        ${mkProxyFrontend {
          name = "fe_proxy_http";
          port = cfg.frontendPorts.fe_proxy_http;
          backend = "be_http";
          tls = false;
        }}
        ${mkProxyFrontend {
          name = "fe_proxy_https";
          port = cfg.frontendPorts.fe_proxy_https;
          backend = "be_https";
          tls = true;
        }}
        backend be_http
            balance source
            hash-type consistent
            option tcp-check
        ${httpServers}
            source ${hostAddr}

        backend be_https
            balance source
            hash-type consistent
            option tcp-check
        ${httpsServers}
            source ${hostAddr}
      '';
    };
  };
}
