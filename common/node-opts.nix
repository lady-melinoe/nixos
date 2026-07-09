{
  lib,
  config,
  pkgs,
  ...
}:
let
  inherit (lib) mkOption types;
  dummyKey = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQCsBCej0Ov40HovFNPphBw2T4aEUjTxxcAqg72oW13ikvyqBLu9OZykbF+5ogVLNRnQuEzpwG1Ur8QiOaYAtak8bDpJY1W8BZJuZkSrAGQHdTs15uRZ0bVpbVLFTQhDG1dazyVubH+F1/pl9jpg2iBKftaBKttV9ua4UqIVfy7bCtxFB5EaJzmiBL1Rj1GatdOYQ8gC3C9O1VLxbwLwFAVeCFCAEqwvuj9nmo/nWZ/vN91/LHFFS6Dh1XaZmAzVAqJz73rRmtc71auUCbwS4a9sCraqUtdU8+ThisIsADummyKey+TransRightsAreHumanRights+BeCrimeDoGay dummy@dummy";
  shellBase64 =
    text:
    let
      drv = pkgs.runCommand "encode-ca-base64" { } ''
        printf %s ${lib.escapeShellArg text} | base64 -w0 > $out
      '';
    in
    builtins.readFile drv;
in
{
  options.melinoe = {
    nodeId = mkOption {
      type = types.nullOr types.int;
      default = null;
      description = "Unique node ID used for addressing and routing.";
    };

    isBuildServer = mkOption {
      type = types.bool;
      default = false;
      description = "Whether this node is allowed to act as a build server.";
    };
    buildMachines = mkOption {
      type = types.listOf (
        types.submodule {
          freeformType = types.attrs;
          options = {
            publicHostCA = mkOption {
              type = types.nullOr types.str;
              default = null;
              description = "The exact @cert-authority line to inject into the machine's publicHostKey.";
            };
          };
        }
      );
      default = [ ];
      description = "Wrapper around nix.buildMachines with SSH CA support.";
    };

    serialMode = mkOption {
      type = types.bool;
      default = false;
      description = "Enable serial console support for EFI/GRUB on this node.";
    };

    extraSerial = mkOption {
      type = types.listOf types.int;
      default = [ ];
      description = "Additional ttyS serial gettys to enable for IPMI-accessible serial consoles.";
    };

    legacyBoot = mkOption {
      type = types.bool;
      default = false;
      description = "Enable legacy GRUB boot settings for this node.";
    };

    internet = mkOption {
      type = types.listOf (
        types.submodule {
          options = {
            ip = mkOption {
              type = types.str;
              description = "Primary IP address for this uplink (use /32 notation).";
            };

            pub_ip = mkOption {
              type = types.nullOr types.str;
              default = null;
              description = "Optional public IP address associated with this uplink.";
            };

            iface = mkOption {
              type = types.listOf types.str;
              description = "Interface names that carry the internet uplink; if multiple interfaces are specified, they will be LACP bonded.";
            };

            bondMode = mkOption {
              type = types.nullOr types.str;
              default = null;
              description = "Bonding mode for a multi-interface uplink (currently only \"lacp\" is supported).";
            };

            lacpRate = mkOption {
              type = types.nullOr types.str;
              default = null;
              description = "LACP rate for a bonded uplink (e.g., \"fast\" or \"slow\").";
            };

            subnet = mkOption {
              type = types.nullOr types.str;
              default = null;
              description = "Subnet for the uplink if applicable.";
            };

            gateway = mkOption {
              type = types.nullOr types.str;
              default = null;
              description = "Gateway address if needed for the uplink.";
            };
          };
        }
      );

      default = [ ];
      description = "Internet uplink definitions; each entry describes an IP, interface, and optional subnet/gateway.";
    };

    advertisedRoutes = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "Additional IPv4 prefixes or host routes to advertise via melinoe-route-list.";
    };

    wgPorts = mkOption {
      type = types.listOf types.int;
      default = [ ];
      description = "UDP ports to allow for WireGuard (derived automatically when using the wireguard module).";
    };

    peers = mkOption {
      type = types.listOf (
        types.submodule {
          options = {
            id = mkOption {
              type = types.int;
              description = "Peer node ID (matches wg-keys mapping).";
            };

            endpoint = mkOption {
              type = types.nullOr types.str;
              default = null;
              description = "Override endpoint hostname/IP for this peer; if null, publicNodes.<id>.defaultEndpoint is used.";
            };

            allowedIPs = mkOption {
              type = types.listOf types.str;
              default = [
                "198.19.3.0/24"
                "198.51.100.0/24"
              ];
              description = "Allowed IPs to route via this peer.";
            };

            persistentKeepalive = mkOption {
              type = types.int;
              default = 25;
              description = "Persistent keepalive in seconds.";
            };
          };
        }
      );

      default = [ ];
      description = "WireGuard peers; used to render interfaces and derive BGP neighbors.";
    };

    publicNodes = mkOption {
      type = types.attrsOf (
        types.submodule {
          options = {
            wgPubkey = mkOption {
              type = types.str;
              description = "Public WireGuard key for the node.";
            };

            defaultEndpoint = mkOption {
              type = types.nullOr types.str;
              default = null;
              description = "Optional default endpoint hostname/IP published by this node.";
            };
          };
        }
      );

      default = { };
      description = "Per-node public artifacts published by nodes/*/public.nix.";
    };

    regions = mkOption {
      type = types.listOf types.str;
      default = [ ];
      description = "Region tags for this node.";
    };
  };

  config = {
    assertions = [
      {
        assertion = config.melinoe.nodeId != null;
        message = "melinoe.nodeId must be set for this host.";
      }
    ]
    ++ lib.concatMap (
      entry:
      let
        isBonded = builtins.length entry.iface > 1;
        hasBondMode = entry.bondMode != null;
        hasLacpRate = entry.lacpRate != null;
      in
      [
        {
          assertion = !(isBonded && !hasBondMode);
          message = "melinoe.internet: bondMode must be set when multiple interfaces are specified.";
        }

        {
          assertion = !(hasBondMode && entry.bondMode != "lacp");
          message = "melinoe.internet: only bondMode = \"lacp\" is supported.";
        }

        {
          assertion = !(hasLacpRate && !isBonded);
          message = "melinoe.internet: lacpRate is only valid when multiple interfaces are specified.";
        }
      ]
    ) config.melinoe.internet
    ++ map (
      peer:
      let
        peerIdStr = toString peer.id;
        hasOverride = peer.endpoint != null;
        hasDefault =
          config.melinoe.publicNodes ? ${peerIdStr}
          && config.melinoe.publicNodes.${peerIdStr}.defaultEndpoint != null;
      in
      {
        assertion = hasOverride || hasDefault;
        message = "melinoe.peers: Peer ID ${peerIdStr} has no endpoint override configured, and no defaultEndpoint is found in publicNodes for this node.";
      }
    ) config.melinoe.peers;

    nix.buildMachines = lib.mkIf (config.melinoe.buildMachines != [ ]) (
      map (
        machine:
        if machine.publicHostCA != null then
          let
            rawPayload = "${dummyKey}\n${machine.publicHostCA}";
            encodedKey = shellBase64 rawPayload;
          in
          (builtins.removeAttrs machine [ "publicHostCA" ]) // { publicHostKey = encodedKey; }
        else
          builtins.removeAttrs machine [ "publicHostCA" ]
      ) config.melinoe.buildMachines
    );
  };
}
