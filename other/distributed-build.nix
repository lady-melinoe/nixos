{
  config,
  lib,
  pkgs,
  ...
}:
let
  inherit (lib) mkOption types;
  cfg = config.melinoe.other.distributedBuild;
  dummyKey = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQCsBCej0Ov40HovFNPphBw2T4aEUjTxxcAqg72oW13ikvyqBLu9OZykbF+5ogVLNRnQuEzpwG1Ur8QiOaYAtak8bDpJY1W8BZJuZkSrAGQHdTs15uRZ0bVpbVLFTQhDG1dazyVubH+F1/pl9jpg2iBKftaBKttV9ua4UqIVfy7bCtxFB5EaJzmiBL1Rj1GatdOYQ8gC3C9O1VLxbwLwFAVeCFCAEqwvuj9nmo/nWZ/vN91/LHFFS6Dh1XaZmAzVAqJz73rRmtc71auUCbwS4a9sCraqUtdU8+ThisIsADummyKey+TransRightsAreHumanRights+BeCrimeDoGay dummy@dummy";
  shellBase64 =
    text:
    let
      drv = pkgs.runCommand "encode-ca-base64" { } ''
        printf %s ${lib.escapeShellArg text} | base64 -w0 > $out
      '';
    in
    builtins.readFile drv;

  resolveBuildMachine =
    machine:
    let
      published =
        if machine.id == null then
          { }
        else
          let
            nodeInfo =
              config.melinoe.other.nodePublicInfo.${toString machine.id}
              or (throw "melinoe.other.distributedBuild.remoteBuildOn: no melinoe.other.nodePublicInfo entry for node id ${toString machine.id}");
          in
          nodeInfo.buildServer
            or (throw "melinoe.other.distributedBuild.remoteBuildOn: node id ${toString machine.id} does not publish buildServer info in its public.nix");
      overrides = lib.filterAttrs (_: v: v != null) (builtins.removeAttrs machine [ "id" ]);
    in
    published // overrides;
in
{
  options.melinoe.other.distributedBuild = {
    isBuildServer = mkOption {
      type = types.bool;
      default = false;
      description = "Whether this node is allowed to act as a build server.";
    };

    remoteBuildOn = mkOption {
      type = types.listOf (
        types.submodule {
          freeformType = types.attrs;
          options = {
            id = mkOption {
              type = types.nullOr types.int;
              default = null;
              description = ''
                Node id to use as a remote builder. When set, hostName,
                publicHostCA, sshUser, system and supportedFeatures default to
                whatever that node publishes via
                melinoe.other.nodePublicInfo.<id>.buildServer (see nodes/*/public.nix);
                set any of those fields explicitly on this entry to override
                the published value. sshKey is never published and must still
                be set here.
              '';
            };
            sshKey = mkOption {
              type = types.str;
              description = "Local path to the SSH private key used to authenticate to this build machine.";
            };
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
  };

  config = lib.mkMerge [
    (lib.mkIf cfg.isBuildServer {
      users.users.remotebuild = {
        isSystemUser = true;
        group = "remotebuild";
        useDefaultShell = true;
      };
      users.groups.remotebuild = { };
      nix.settings.trusted-users = [
        "remotebuild"
      ];
    })
    (lib.mkIf (cfg.remoteBuildOn != [ ]) {
      nix.buildMachines = map (
        machine:
        let
          resolved = resolveBuildMachine machine;
        in
        if (resolved.publicHostCA or null) != null then
          let
            rawPayload = "${dummyKey}\n${resolved.publicHostCA}";
            encodedKey = shellBase64 rawPayload;
          in
          (builtins.removeAttrs resolved [ "publicHostCA" ]) // { publicHostKey = encodedKey; }
        else
          builtins.removeAttrs resolved [ "publicHostCA" ]
      ) cfg.remoteBuildOn;
    })
  ];
}
