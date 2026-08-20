{
  config,
  lib,
  melinoeNodeIntraIP,
  ...
}:
let
  inherit (lib) mkOption types;
  mel = config.melinoe;
  cfg = mel.services.haproxy;
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
  options.melinoe.services.haproxy = {
    enable = mkOption {
      type = types.bool;
      default = true;
      description = "Whether to run the melinoe haproxy frontend/backend setup on this node.";
    };

    backendNodes = mkOption {
      type = types.listOf (
        types.submodule {
          options = {
            name = mkOption {
              type = types.str;
              description = "haproxy server name for this backend node.";
            };
            address = mkOption {
              type = types.str;
              description = "IP address of this backend node (fronted on the fe_proxy_* ports).";
            };
          };
        }
      );
      default = [ ];
      description = "Backend web nodes load-balanced by the be_http/be_https backends.";
    };

    proxyProtocolAllowedSources = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = ''
        Source IPs allowed to speak PROXY protocol on the fe_proxy_http/fe_proxy_https
        frontends - i.e. the upstream haproxy/load-balancer boxes.

        Access logging on those frontends is also silenced for any connection
        whose PROXY-protocol-encoded source is itself one of these addresses,
        since that combination (allowed upstream, proxying itself as the
        client) only occurs on an upstream's own health-check, never on
        forwarded real traffic. Leave empty to disable both the allow-list
        and the log silencing.
      '';
    };

    frontendPorts = {
      fe_http = mkOption {
        type = types.port;
        default = 80;
        description = "Bind port for the plain HTTP frontend.";
      };
      fe_https = mkOption {
        type = types.port;
        default = 443;
        description = "Bind port for the plain HTTPS frontend.";
      };
      fe_http_prxbp = mkOption {
        type = types.port;
        default = 6080;
        description = "Bind port for the HTTP frontend fronting proxy-bypass traffic.";
      };
      fe_https_prxbp = mkOption {
        type = types.port;
        default = 6443;
        description = "Bind port for the HTTPS frontend fronting proxy-bypass traffic.";
      };
      fe_proxy_http = mkOption {
        type = types.port;
        default = 1080;
        description = "Bind port for the HTTP frontend expecting PROXY protocol.";
      };
      fe_proxy_https = mkOption {
        type = types.port;
        default = 1443;
        description = "Bind port for the HTTPS frontend expecting PROXY protocol.";
      };
    };

    ports = mkOption {
      type = types.listOf types.port;
      readOnly = true;
      default = lib.attrValues config.melinoe.services.haproxy.frontendPorts;
      description = "TCP ports bound by the haproxy frontends; consumed by melinoe.node.networking.openPorts.";
    };
  };

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
