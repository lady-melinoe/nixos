{
  config,
  lib,
  pkgs,
  ...
}:
let
  inherit (lib) mkOption types;
  cfg = config.melinoe.build;
  nodes = config.melinoe.cluster.nodes;
  self = config.networking.hostName;

  # nix has no base64 builtin: https://github.com/NixOS/nix/issues/16117
  base64 =
    text:
    builtins.readFile (
      pkgs.runCommand "base64" { } ''
        printf %s ${lib.escapeShellArg text} | base64 -w0 > $out
      ''
    );

  # nix writes publicHostKey out as "<hostName> <key>"; the CA line rides in behind a placeholder
  placeholderHostKey = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQCsBCej0Ov40HovFNPphBw2T4aEUjTxxcAqg72oW13ikvyqBLu9OZykbF+5ogVLNRnQuEzpwG1Ur8QiOaYAtak8bDpJY1W8BZJuZkSrAGQHdTs15uRZ0bVpbVLFTQhDG1dazyVubH+F1/pl9jpg2iBKftaBKttV9ua4UqIVfy7bCtxFB5EaJzmiBL1Rj1GatdOYQ8gC3C9O1VLxbwLwFAVeCFCAEqwvuj9nmo/nWZ/vN91/LHFFS6Dh1XaZmAzVAqJz73rRmtc71auUCbwS4a9sCraqUtdU8+ThisIsADummyKey+TransRightsAreHumanRights+BeCrimeDoGay dummy@dummy";

  publicHostKey = server: base64 "${placeholderHostKey}\n${server.publicHostCA}";

  builder =
    name:
    let
      server = nodes.${name}.buildServer;
    in
    {
      inherit (server)
        hostName
        sshUser
        system
        supportedFeatures
        ;
      inherit (cfg) sshKey;
      publicHostKey = publicHostKey server;
    };

  substituter =
    name:
    let
      server = nodes.${name}.buildServer;
      query = lib.concatStringsSep "&" (
        lib.mapAttrsToList (k: v: "${k}=${lib.escapeURL v}") {
          ssh-key = cfg.sshKey;
          base64-ssh-public-host-key = publicHostKey server;
          trusted = "1";
        }
      );
    in
    "ssh-ng://${server.sshUser}@${server.hostName}?${query}";

  buildServers = lib.attrNames (lib.filterAttrs (_: n: n.buildServer.enable) nodes);
  sshUser = nodes.${self}.buildServer.sshUser;
in
{
  options.melinoe.build = {
    remote = mkOption {
      type = types.listOf (types.enum (lib.attrNames nodes));
      default = [ ];
      description = "Nodes to offload builds to.";
    };
    sshKey = mkOption {
      type = types.str;
      default = "/root/.ssh/id_remotebuild";
      description = "Key used to log in to the remote builders.";
    };
    substituteFrom = mkOption {
      type = types.listOf (types.enum (lib.attrNames nodes));
      default = lib.optionals nodes.${self}.buildServer.enable (lib.remove self buildServers);
      defaultText = lib.literalMD "the other build servers, if this node is one; otherwise `[ ]`";
      description = "Nodes (with sshStore.enable) whose stores are used as trusted (unsigned) ssh-ng substituters.";
    };
  };

  config = lib.mkMerge [
    (lib.mkIf (nodes.${self}.sshStore.enable || nodes.${self}.buildServer.enable) {
      users.users.${sshUser} = {
        isSystemUser = true;
        group = sshUser;
        useDefaultShell = true;
      };
      users.groups.${sshUser} = { };
    })

    (lib.mkIf (nodes.${self}.buildServer.enable) {
      nix.settings.trusted-users = [ sshUser ];
    })

    (lib.mkIf (cfg.remote != [ ]) {
      assertions = map (name: {
        assertion = nodes.${name}.buildServer.enable;
        message = "melinoe.build.remote: ${name} does not offer builds (set buildServer.enable in nodes/${name}/public.nix).";
      }) cfg.remote;

      nix.distributedBuilds = true;
      nix.settings.builders-use-substitutes = true;
      nix.buildMachines = map builder cfg.remote;
    })

    (lib.mkIf (cfg.substituteFrom != [ ]) {
      assertions = map (name: {
        assertion = nodes.${name}.sshStore.enable && name != self;
        message = "melinoe.build.substituteFrom: ${name} is this node or does not serve its store (set sshStore.enable in nodes/${name}/public.nix).";
      }) cfg.substituteFrom;

      nix.settings.substituters = map substituter cfg.substituteFrom;

      programs.ssh.extraConfig = ''
        Host ${lib.concatMapStringsSep " " (name: nodes.${name}.buildServer.hostName) cfg.substituteFrom}
          ConnectTimeout 5
      '';
    })
  ];
}
