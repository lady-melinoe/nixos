{
  pkgs,
  lib,
  nixosConfigurations,
}:

let
  stripCidr = ip: lib.head (lib.splitString "/" ip);

  hostMeta = lib.mapAttrs (
    name: cfg:
    let
      c = cfg.config;
      nodeId = toString c.melinoe.node.id;

      internetIps = lib.concatMap (
        uplink:
        [ uplink.ip ]
        ++ lib.optional (uplink.pub_ip != null) uplink.pub_ip
      ) c.melinoe.node.networking.uplinks;
    in
    {
      hostname = c.networking.hostName;
      sshTarget = "${name}.infra.melinoe.xyz";

      # Paths (on this host) of any keys it uses to ssh out to build
      # servers, per `melinoe.node.remoteBuildOn`. These are declared in
      # nix per-node rather than assumed, since the key name/path isn't
      # fixed across nodes. `sshKey` is a freeform field on the
      # remoteBuildOn submodule, so it may be absent.
      remoteBuildKeys = lib.unique (
        builtins.filter (k: k != null) (map (m: m.sshKey or null) c.melinoe.node.remoteBuildOn)
      );

      principals = lib.unique (
        [
          c.networking.hostName
          "${c.networking.hostName}.intra.melinoe.xyz"
          "${c.networking.hostName}.infra.melinoe.xyz"
        ]
        ++ map stripCidr internetIps
        ++ [
          "198.19.3.${nodeId}"
          "198.18.0.${nodeId}"
        ]
      );
    }
  ) nixosConfigurations;

  hostMetaJsonFile = pkgs.writeText "mel-ssh-provision-hosts.json" (builtins.toJSON hostMeta);

  script = pkgs.writeText "mel-ssh-provision.py" (builtins.readFile ./mel-ssh-provision.py);
in
pkgs.writeShellApplication {
  name = "mel-ssh-provision";

  runtimeInputs = with pkgs; [
    openssh
    python3
  ];

  text = ''
    export MEL_HOSTS_JSON_PATH="${hostMetaJsonFile}"
    exec python3 "${script}" "$@"
  '';
}
