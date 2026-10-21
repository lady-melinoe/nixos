{
  config,
  lib,
  melinoeLib,
  ...
}:
let
  inherit (lib) mkOption types;
  cfg = config.melinoe.haproxy;

  proxyHttp = 1080;
  proxyHttps = 1443;

  allowedSources = lib.concatStringsSep " " cfg.proxyProtocolSources;

  frontends = [
    {
      name = "fe_http";
      port = 80;
      backend = "be_http";
    }
    {
      name = "fe_https";
      port = 443;
      backend = "be_https";
      tls = true;
    }
    {
      name = "fe_http_prxbp";
      port = 6080;
      backend = "be_http";
    }
    {
      name = "fe_https_prxbp";
      port = 6443;
      backend = "be_https";
      tls = true;
    }
    {
      name = "fe_proxy_http";
      port = proxyHttp;
      backend = "be_http";
      proxy = true;
    }
    {
      name = "fe_proxy_https";
      port = proxyHttps;
      backend = "be_https";
      proxy = true;
      tls = true;
    }
  ];

  renderFrontend =
    {
      name,
      port,
      backend,
      proxy ? false,
      tls ? false,
    }:
    lib.concatStringsSep "\n" (
      [
        "frontend ${name}"
        "    bind *:${toString port}"
      ]
      ++ lib.optionals proxy (
        [
          "    tcp-request connection reject if !{ src ${allowedSources} }"
          "    tcp-request connection expect-proxy layer4 if { src ${allowedSources} }"
        ]
        ++ lib.optional (
          cfg.proxyProtocolSources != [ ]
        ) "    tcp-request content set-log-level silent if { src ${allowedSources} }"
      )
      ++ lib.optionals tls [
        "    tcp-request inspect-delay 5s"
        "    tcp-request content accept if { req_ssl_hello_type 1 }"
      ]
      ++ [ "    default_backend ${backend}" ]
    );

  renderBackend =
    name: port:
    lib.concatStringsSep "\n" (
      [
        "backend ${name}"
        "    balance source"
        "    hash-type consistent"
        "    option tcp-check"
      ]
      ++ lib.mapAttrsToList (
        server: address:
        "    server ${server} ${address}:${toString port} send-proxy check check-send-proxy"
      ) cfg.backends
      ++ [ "    source ${config.melinoe.node.address}" ]
    );
in
{
  options.melinoe.haproxy = {
    enable = mkOption {
      type = types.bool;
      default = true;
    };
    backends = mkOption {
      type = types.attrsOf types.str;
      default = { };
      description = "Web nodes to balance across, by server name and address.";
    };
    proxyProtocolSources = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = ''
        Upstream load balancers allowed to speak PROXY protocol. Connections
        proxied on behalf of one of these (their own health checks) aren't logged.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    melinoe.firewall.openPorts.tcp = map (frontend: frontend.port) frontends;

    systemd.services.haproxy = melinoeLib.afterMeshAddress;

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

        ${lib.concatMapStringsSep "\n\n" renderFrontend frontends}

        ${renderBackend "be_http" proxyHttp}

        ${renderBackend "be_https" proxyHttps}
      '';
    };
  };
}
