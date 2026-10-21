{
  config,
  lib,
  pkgs,
  ...
}:
let
  update = pkgs.writeShellApplication {
    name = "update";
    runtimeInputs = [ pkgs.git ];
    text = builtins.readFile ./update/update.sh;
  };

  updateSafe = pkgs.writeShellApplication {
    name = "update-safe";
    text = ''
      ${builtins.readFile ./update/update-safe.sh}
      exec ${lib.getExe update} --only-if-updated
    '';
  };
in
{
  options.melinoe.update.remote = lib.mkOption {
    type = lib.types.bool;
    default = true;
    description = "Let gitlab-deploy run update-safe over SSH. mel-deploy deploys to the nodes where this is set.";
  };

  config = lib.mkMerge [
    {
      environment.systemPackages = [
        update
        updateSafe
      ];
    }

    (lib.mkIf config.melinoe.update.remote {
      users.users.gitlab-deploy = {
        isSystemUser = true;
        group = "gitlab-deploy";
        home = "/var/empty";
        createHome = false;
        shell = pkgs.bash;
        hashedPassword = "!";
      };
      users.groups.gitlab-deploy = { };

      security.sudo = {
        extraRules = [
          {
            users = [ "gitlab-deploy" ];
            commands = [
              {
                command = "/run/current-system/sw/bin/update-safe";
                options = [ "NOPASSWD" ];
              }
            ];
          }
        ];
        extraConfig = ''
          Defaults:gitlab-deploy env_reset
          Defaults:gitlab-deploy env_delete+="SSH_AUTH_SOCK SSH_CLIENT SSH_CONNECTION SSH_ORIGINAL_COMMAND SSH_TTY"
          Defaults:gitlab-deploy secure_path="/run/current-system/sw/bin"
        '';
      };
    })
  ];
}
