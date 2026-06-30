{ config, lib, ... }:

{
  config = lib.mkIf config.melinoe.isBuildServer {
    users.users.remotebuild = {
      isSystemUser = true;
      group = "remotebuild";
      useDefaultShell = true;
    };

    users.groups.remotebuild = { };

    nix.settings.trusted-users = [
      "remotebuild"
    ];
  };
}
