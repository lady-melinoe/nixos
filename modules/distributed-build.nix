{
  config,
  lib,
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
  config = lib.mkMerge [
    (lib.mkIf config.melinoe.node.isBuildServer {
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
    (lib.mkIf (config.melinoe.node.remoteBuildOn != [ ]) {
      nix.buildMachines = map (
        machine:
        if machine.publicHostCA != null then
          let
            rawPayload = "${dummyKey}\n${machine.publicHostCA}";
            encodedKey = shellBase64 rawPayload;
          in
          (builtins.removeAttrs machine [ "publicHostCA" ]) // { publicHostKey = encodedKey; }
        else
          builtins.removeAttrs machine [ "publicHostCA" ]
      ) config.melinoe.node.remoteBuildOn;
    })
  ];
}
