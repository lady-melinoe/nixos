{
  lib,
  pkgs,
  config,
  ...
}:
let
  inherit (lib) mkOption mkIf types;

  update = pkgs.writeShellScriptBin "update" ''
        #!/usr/bin/env bash
        set -euo pipefail

        no_pull=0
        no_verify=0
        only_if_updated=0
        rebuild_mode="switch"
        hostname=""

        usage() {
          cat <<'EOF'
    Usage: update [--no-pull] [--no-verify] [--only-if-updated] [--boot] [--hostname HOSTNAME]

    Options:
      --no-pull            Skip git pull in /etc/nixos
      --no-verify          Skip verification of the fetched commit signature
      --only-if-updated    Only rebuild if git merge changed the checkout
      --boot               Run nixos-rebuild boot instead of switch
      --hostname NAME      Rebuild a different host than the local machine
      -h, --help           Show this help
    EOF
        }

        while [ "$#" -gt 0 ]; do
          case "$1" in
            --no-pull)
              no_pull=1
              ;;
            --no-verify)
              no_verify=1
              ;;
            --only-if-updated)
              only_if_updated=1
              ;;
            --boot)
              rebuild_mode="boot"
              ;;
            --hostname)
              shift
              if [ "$#" -eq 0 ]; then
                echo "--hostname requires a value" >&2
                exit 1
              fi
              hostname="$1"
              ;;
            -h|--help)
              usage
              exit 0
              ;;
            *)
              echo "Unknown option: $1" >&2
              usage >&2
              exit 1
              ;;
          esac
          shift
        done

        if [ -z "$hostname" ]; then
          hostname="$(hostname -s)"
        fi

        rebuild=1

        if [ "$no_pull" -eq 0 ]; then
          repo=/etc/nixos
          allowed_signers="$repo/allowed_signers"
          upstream="$(git -C "$repo" rev-parse --abbrev-ref --symbolic-full-name '@{u}')"

          before="$(git -C "$repo" rev-parse HEAD)"

          git -C "$repo" fetch
          upstream_commit="$(git -C "$repo" rev-parse --verify "$upstream^{commit}")"

          if [ "$no_verify" -eq 0 ]; then
            git -C "$repo" -c "gpg.ssh.allowedSignersFile=$allowed_signers" verify-commit "$upstream_commit"
          fi

          git -C "$repo" merge --ff-only "$upstream_commit"

          after="$(git -C "$repo" rev-parse HEAD)"

          if [ "$only_if_updated" -eq 1 ] && [ "$before" = "$after" ]; then
            rebuild=0
          fi
        fi

        if [ "$rebuild" -eq 0 ]; then
          echo "No changes pulled; skipping rebuild."
          exit 0
        fi

        exec nixos-rebuild "$rebuild_mode" --flake "/etc/nixos/#''${hostname}"
  '';
  update-safe = pkgs.writeShellScriptBin "update-safe" ''
    #!/usr/bin/env bash
    set -euo pipefail

    exec 0</dev/null

    unset LD_PRELOAD LD_LIBRARY_PATH PYTHONPATH PERL5LIB \
          SUDO_COMMAND SUDO_USER SUDO_UID SUDO_GID \
          LANG

    unset GIT_DIR GIT_WORK_TREE GIT_EXEC_PATH \
          GIT_SSH GIT_SSH_COMMAND GIT_TEMPLATE_DIR \
          GIT_CONFIG_COUNT GIT_NAMESPACE

    export GIT_CONFIG_NOSYSTEM=1
    export GIT_CONFIG_GLOBAL=/dev/null

    export GIT_PAGER=cat

    export LC_ALL=C
    export PATH="/run/current-system/sw/bin:/usr/bin:/bin"

    exec ${update}/bin/update --only-if-updated
  '';
in
{
  config = mkIf config.melinoe.node.isRemoteUpdatable {
    environment.systemPackages = [
      update
      update-safe
    ];

    users.users.gitlab-deploy = {
      isSystemUser = true;
      group = "gitlab-deploy";
      home = "/var/empty";
      createHome = false;
      shell = pkgs.bash;
      hashedPassword = "!";
    };
    users.groups.gitlab-deploy = { };

    security.sudo.extraRules = [
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
    security.sudo.extraConfig = ''
      Defaults:gitlab-deploy env_reset
      Defaults:gitlab-deploy env_delete+="SSH_AUTH_SOCK SSH_CLIENT SSH_CONNECTION SSH_ORIGINAL_COMMAND SSH_TTY"
      Defaults:gitlab-deploy secure_path="/run/current-system/sw/bin"
    '';
  };
}
