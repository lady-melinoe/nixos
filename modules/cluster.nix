{
  config,
  lib,
  melinoeLib,
  ...
}:
let
  inherit (lib) mkOption types;
  cfg = config.melinoe.cluster;

  nodeModule =
    { name, ... }:
    {
      options = {
        id = mkOption {
          type = types.ints.between 1 254;
          description = "Unique id; selects the node's address in networks.nodes and its policy-routing table.";
        };
        pubkey = mkOption {
          type = types.str;
          description = "Base64 Curve25519 public key, same format as a WireGuard key.";
        };
        endpoint = mkOption {
          type = types.nullOr types.str;
          default = null;
          description = "IP literal other nodes dial. Null for nodes that only dial out.";
        };
        buildServer = mkOption {
          default = { };
          description = "Offer this node as a remote builder.";
          type = types.submodule {
            options = {
              enable = mkOption {
                type = types.bool;
                default = false;
              };
              hostName = mkOption {
                type = types.str;
                default = "${name}.infra.melinoe.xyz";
              };
              publicHostCA = mkOption {
                type = types.str;
                default = "@cert-authority ${lib.concatStringsSep "," cfg.ssh.builderHostPatterns}, ${cfg.ssh.hostCA} SSH Host CA";
              };
              sshUser = mkOption {
                type = types.str;
                default = "remotebuild";
              };
              system = mkOption {
                type = types.str;
                default = "x86_64-linux";
              };
              supportedFeatures = mkOption {
                type = types.listOf types.str;
                default = [
                  "nixos-test"
                  "big-parallel"
                  "kvm"
                ];
              };
            };
          };
        };
      };
    };

  vmModule =
    { name, config, ... }:
    {
      options = {
        iface = mkOption {
          type = types.str;
          default = "vm-${name}";
        };
        ip = mkOption {
          type = types.str;
          description = "Address of the VM, or CIDR for a VM that bridges a whole subnet.";
        };
        address = mkOption {
          type = types.str;
          readOnly = true;
          default = lib.head (lib.splitString "/" config.ip);
        };
        source = mkOption {
          type = types.str;
          readOnly = true;
          default = if lib.hasInfix "/" config.ip then config.ip else "${config.ip}/32";
        };
        tcp = mkOption {
          type = types.listOf types.port;
          default = [ ];
          description = "TCP ports forwarded to this VM.";
        };
        udp = mkOption {
          type = types.listOf types.port;
          default = [ ];
          description = "UDP ports forwarded to this VM.";
        };
        outboundVia = mkOption {
          type = types.nullOr (types.enum (lib.attrNames cfg.nodes));
          default = null;
          description = "Node this VM's outbound traffic leaves the mesh through.";
        };
        hostAccess = melinoeLib.mkAccessRuleOption "Ports and protocols this VM may reach on any node itself.";
        outboundDropIP = mkOption {
          type = types.listOf types.str;
          default = [ ];
          description = "Addresses and CIDRs this VM may not reach.";
        };
        outboundAllowOnlyIP = mkOption {
          type = types.listOf types.str;
          default = [ ];
          description = "If non-empty, the only addresses and CIDRs this VM may reach.";
        };
      };
    };
in
{
  options.melinoe.cluster = {
    networks = {
      nodes = mkOption {
        type = types.str;
        default = "198.18.0.0/24";
        description = "Range node addresses are drawn from.";
      };
      mesh = mkOption {
        type = types.str;
        default = "198.18.0.0/16";
        description = "Everything reachable over the mesh: nodes, containers and VMs.";
      };
    };

    nodes = mkOption {
      type = types.attrsOf (types.submodule nodeModule);
      default = { };
    };

    vms = mkOption {
      type = types.attrsOf (types.submodule vmModule);
      default = { };
      description = "Incus guests; drives interface setup, forwarding and the firewall.";
    };

    ssh = {
      userCA = mkOption { type = types.str; };
      hostCertPath = mkOption {
        type = types.str;
        default = "/etc/ssh/ssh_host_ed25519_key-cert.pub";
        description = "Where each node keeps its host certificate; mel-ssh-provision writes it there.";
      };
      hostCA = mkOption {
        type = types.str;
        description = "Host CA public key, without a comment.";
      };
      hostPatterns = mkOption {
        type = types.listOf types.str;
        description = "Host patterns the host CA is trusted for.";
      };
      builderHostPatterns = mkOption {
        type = types.listOf types.str;
        description = "Host patterns the host CA is trusted for when connecting to remote builders.";
      };
    };
  };
}
