# What the melnode module refuses at evaluation time, without booting anything:
#
#   nix build .#checks.x86_64-linux.melnode-assertions -L
{
  inputs,
  pkgs,
  lib,
  melinoeLib,
}:
let
  inherit (import ./nodes.nix { inherit lib; }) node;

  # The messages of melnode's failed assertions for node a configured with these peers.
  failures =
    peers:
    let
      system = lib.nixosSystem {
        specialArgs = { inherit inputs melinoeLib; };
        modules = [
          (node "a")
          {
            networking.hostName = "a";
            melinoe.melnode.peers = lib.mkForce peers;
          }
        ];
      };
    in
    map (assertion: assertion.message) (
      lib.filter (
        assertion: !assertion.assertion && lib.hasPrefix "melinoe.melnode" assertion.message
      ) system.config.assertions
    );

  # peers, and the part of the message the one failed assertion has (null: none fail).
  cases = {
    "an address" = {
      peers.b.endpoint = "192.0.2.7";
      fails = null;
    };
    "the node's own endpoint" = {
      peers.b = 0;
      fails = null;
    };
    "an IPv6 address" = {
      peers.b.endpoint = "2001:db8::7";
      fails = null;
    };
    "a WireGuard style ip:port" = {
      peers.b.endpoint = "192.0.2.7:51820";
      fails = "must be an IPv4 or IPv6 address";
    };
    "a hostname" = {
      peers.b.endpoint = "peer.example.org";
      fails = "must be an IPv4 or IPv6 address";
    };
    "an octet out of range" = {
      peers.b.endpoint = "192.0.2.256";
      fails = "must be an IPv4 or IPv6 address";
    };
    "an IPv6 zone" = {
      peers.b.endpoint = "fe80::7%eth0";
      fails = "must be an IPv4 or IPv6 address";
    };
    "a bracketed IPv6 with a port" = {
      peers.b.endpoint = "[2001:db8::7]:60198";
      fails = "must be an IPv4 or IPv6 address";
    };
    "a node that does not exist" = {
      peers.nobody = 0;
      fails = "is not in melinoe.cluster.nodes";
    };
    "the node itself" = {
      peers.a = 0;
      fails = "lists itself as a peer";
    };
  };

  verdict =
    name:
    { peers, fails }:
    let
      got = failures peers;
      ok = if fails == null then got == [ ] else lib.length got == 1 && lib.hasInfix fails (lib.head got);
    in
    lib.optional (!ok) "${name}: expected ${if fails == null then "no failure" else "'${fails}'"}, got ${builtins.toJSON got}";

  # prependCount is a module option type: 254 evaluates, 255 does not.
  prependAccepted =
    count:
    (builtins.tryEval (
      (lib.nixosSystem {
        specialArgs = { inherit inputs melinoeLib; };
        modules = [
          (node "a")
          {
            networking.hostName = "a";
            melinoe.melnode.peers = lib.mkForce { b = count; };
          }
        ];
      }).config.melinoe.melnode.peers.b.prependCount
    )).success;

  problems =
    lib.concatLists (lib.mapAttrsToList verdict cases)
    ++ lib.optional (!prependAccepted 254) "prependCount 254 was refused"
    ++ lib.optional (prependAccepted 255) "prependCount 255 was accepted";
in
pkgs.runCommand "melnode-assertions" { } (
  if problems == [ ] then
    "touch $out"
  else
    ''
      cat >&2 <<'EOF2'
      ${lib.concatStringsSep "\n" problems}
      EOF2
      exit 1
    ''
)
