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
      publicHostKey = base64 "${placeholderHostKey}\n${server.publicHostCA}";
    };
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
  };

  config = lib.mkMerge [
    (lib.mkIf (nodes.${config.networking.hostName}.buildServer.enable) {
      users.users.remotebuild = {
        isSystemUser = true;
        group = "remotebuild";
        useDefaultShell = true;
      };
      users.groups.remotebuild = { };
      nix.settings.trusted-users = [ "remotebuild" ];
    })

    (lib.mkIf (cfg.remote != [ ]) {
      assertions = map (name: {
        assertion = nodes.${name}.buildServer.enable;
        message = "melinoe.build.remote: ${name} does not offer builds (set buildServer.enable in hosts/${name}/public.nix).";
      }) cfg.remote;

      nix.distributedBuilds = true;
      nix.settings.builders-use-substitutes = true;
      nix.buildMachines = map builder cfg.remote;
    })
  ];
}
