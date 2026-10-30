# The nodes of the melnode tests: three nodes in a line (a - b - c), so a and c only reach each other
# through b's relayed route, each running the real melnode module.
{ lib }:
let
  # Throwaway keys, only ever used by this test.
  nodes = {
    a = {
      id = 1;
      private = "U6h+b9at5XYDow/lfxxKZ49ASaokShh3WbF4dGJ7Z1g=";
      pubkey = "RRUWQhiWEqztWKeofDH1asOILEo5qbdUJ38XOJAIlVs=";
      peers = [ "b" ];
    };
    b = {
      id = 2;
      private = "PBMDT6OtREuGeA7Aqr/gH4S7kwQp2j/X5xwUrHBhzg8=";
      pubkey = "rzPdTW6ErN7BA9z3rQhgfvc/hgKPMWyJ4i8NHky9ZWc=";
      peers = [
        "a"
        "c"
      ];
    };
    c = {
      id = 3;
      private = "PHCp15zp36Xdy/WVT+zhN9lYHOgX2cCqJMP0t61L5RY=";
      pubkey = "KUaz3Xm2zHOpHMWi4DlLUHkekPYvbFflA8G0O3G84QA=";
      peers = [ "b" ];
    };
  };

  # The test driver numbers the nodes' addresses on vlan 1 in alphabetical order, which is id order.
  underlay = name: "192.168.1.${toString nodes.${name}.id}";

  node =
    name:
    { pkgs, ... }:
    {
      imports = [
        ../../modules/cluster.nix
        ../../modules/node.nix
        ../../modules/base/boot.nix
        ../../modules/net/options.nix
        ../../modules/net/uplink.nix
        ../../modules/net/firewall.nix
        ../../modules/net/melnode.nix
      ];

      # melnode.nix only asks whether Incus runs.
      options.melinoe.incus.enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
      };

      config = {
        nixpkgs.hostPlatform = "x86_64-linux";

        melinoe = {
          cluster.nodes = lib.mapAttrs (n: v: {
            inherit (v) id pubkey;
            endpoint = underlay n;
          }) nodes;

          uplinks = [
            {
              interfaces = [ "eth1" ];
              address = "${underlay name}/24";
            }
          ];

          melnode.peers = lib.genAttrs nodes.${name}.peers (_: 0);

          # firewall.nix writes an empty (invalid) nft set when there are no VMs.
          cluster.vms.placeholder.ip = "198.18.1.1";
        };

        environment.etc."melinoe/wg.privatekey" = {
          text = nodes.${name}.private;
          mode = "0600";
        };

        environment.systemPackages = [ pkgs.jq ];

        # The same node with a different configuration, to start onto the state a crash left behind.
        specialisation =
          lib.optionalAttrs (name == "b") {
            without-c.configuration.melinoe.melnode.peers = lib.mkForce { a = 0; };
          }
          // lib.optionalAttrs (name == "a") {
            mtu-1400.configuration.melinoe.melnode.mtu = 1400;
          };
      };
    };
in
{ inherit nodes node; }
