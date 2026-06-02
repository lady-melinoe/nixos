{
  description = "NixOS configs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    disko.url = "github:nix-community/disko";
    deploy-rs.url = "github:serokell/deploy-rs";
  };

  outputs =
    {
      self,
      nixpkgs,
      disko,
      deploy-rs,
      ...
    }@inputs:
    let
      system = "x86_64-linux";
      lib = nixpkgs.lib;
      pkgs = import nixpkgs {
        inherit system;
      };

      nodeDir = ./nodes;
      nodes =
        let
          dirEntries = builtins.readDir nodeDir;
          nodeNames = lib.filterAttrs (
            name: type: type == "directory" && builtins.pathExists (nodeDir + "/${name}/node.nix")
          ) dirEntries;
        in
        lib.mapAttrs (name: _: nodeDir + "/${name}/node.nix") nodeNames;

      mkNixosConfiguration =
        _: module:
        lib.nixosSystem {
          inherit system;
          specialArgs = { inherit inputs system; };
          modules = [ module ];
        };

      mkDeployNode = name: _: {
        hostname = "${name}.infra.melinoe.xyz";
        profiles.system = {
          user = "root";
          path = deploy-rs.lib.${system}.activate.nixos self.nixosConfigurations.${name};
        };
      };

      nixosConfigurations = lib.mapAttrs mkNixosConfiguration nodes;
      deployNodes = lib.mapAttrs mkDeployNode nodes;

      hostMeta =
        let
          stripCidr = ip: lib.head (lib.splitString "/" ip);
        in
        lib.mapAttrs (
          name: cfg:
          let
            c = cfg.config;
            nodeId = toString c.melinoe.nodeId;
            internetIps = lib.concatMap (
              uplink: [ uplink.ip ] ++ lib.optional (uplink.pub_ip != null) uplink.pub_ip
            ) c.melinoe.internet;
          in
          {
            hostname = c.networking.hostName;
            sshTarget = "${name}.infra.melinoe.xyz";
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
    in
    {
      formatter.x86_64-linux = nixpkgs.legacyPackages.${system}.nixfmt-tree;
      packages.${system} = {
        borg-beta = pkgs.stdenvNoCC.mkDerivation {
          pname = "borg-beta";
          version = "2.0.0b21";

          src = pkgs.fetchurl {
            url = "https://github.com/borgbackup/borg/releases/download/2.0.0b21/borg-linux-glibc235-x86_64-gh";
            hash = "sha256-HN5TudJIwaYA32+TjuoyB/JxQb4OjXqAVC+o+QHyIcU=";
          };

          dontUnpack = true;
          dontBuild = true;

          installPhase = ''
            runHook preInstall
            install -Dm755 "$src" "$out/bin/borg"
            runHook postInstall
          '';
        };

        mel-ssh-host-ca =
          let
            hostMetaJson = builtins.toJSON hostMeta;
          in
          pkgs.writeShellApplication {
            name = "mel-ssh-host-ca";

            runtimeInputs = [
              pkgs.coreutils
              pkgs.jq
              pkgs.openssh
            ];

            # The generated script intentionally builds remote shell snippets from
            # local constants. Avoid making ShellCheck warnings fail the Nix build.
            checkPhase = "";

            text = ''
                            set -euo pipefail

                            hosts_json='${hostMetaJson}'
                            ca_key="''${MEL_HOST_CA_KEY:-$HOME/ssh-ca-keys/ssh-host-ca}"
                            validity="''${MEL_HOST_CERT_VALIDITY:-+30d}"
                            workdir="''${MEL_HOST_CA_WORKDIR:-.ssh-host-ca-work}"
                            host_key_path="''${MEL_HOST_KEY_PATH:-/etc/ssh/ssh_host_ed25519_key}"
                            host_pubkey_path="''${MEL_HOST_PUBKEY_PATH:-/etc/ssh/ssh_host_ed25519_key.pub}"
                            host_cert_path="''${MEL_HOST_CERT_PATH:-/etc/ssh/ssh_host_ed25519_key-cert.pub}"

                            usage() {
                              cat <<'EOF'
              Usage:
                mel-ssh-host-ca list
                mel-ssh-host-ca principals HOST
                mel-ssh-host-ca bootstrap HOST
                mel-ssh-host-ca retrieve HOST
                mel-ssh-host-ca sign HOST [PUBKEY]
                mel-ssh-host-ca deploy HOST [CERT]
                mel-ssh-host-ca rotate [--bootstrap] HOST
                mel-ssh-host-ca rotate-all [--bootstrap]

              Environment:
                MEL_HOST_CA_KEY=/path/to/ssh-host-ca
                MEL_HOST_CERT_VALIDITY=+30d
                MEL_HOST_CA_WORKDIR=.ssh-host-ca-work
              EOF
                            }

                            names() {
                              printf '%s\n' "$hosts_json" | jq -r 'keys[]' | sort
                            }

                            require_host() {
                              local host="$1"
                              if ! printf '%s\n' "$hosts_json" | jq -e --arg host "$host" 'has($host)' >/dev/null; then
                                echo "Unknown host: $host" >&2
                                echo "Known hosts:" >&2
                                names >&2
                                exit 1
                              fi
                            }

                            ssh_target() {
                              printf '%s\n' "$hosts_json" | jq -r --arg host "$1" '.[$host].sshTarget'
                            }

                            principals_csv() {
                              printf '%s\n' "$hosts_json" | jq -r --arg host "$1" '.[$host].principals | join(",")'
                            }

                            host_workdir() {
                              printf '%s/%s\n' "$workdir" "$1"
                            }

                            ask_yes_no() {
                              local prompt="$1"
                              local answer

                              if [ ! -t 0 ]; then
                                return 1
                              fi

                              while true; do
                                printf '%s [y/N] ' "$prompt" >&2
                                read -r answer
                                case "$answer" in
                                  y|Y|yes|YES) return 0 ;;
                                  n|N|no|NO|"") return 1 ;;
                                  *) echo "Please answer y or n." >&2 ;;
                                esac
                              done
                            }

                            bootstrap() {
                              local host="$1"
                              local target

                              require_host "$host"
                              target="$(ssh_target "$host")"

                              ssh "root@$target" "
                                set -euo pipefail
                                if [ ! -s '$host_key_path' ]; then
                                  rm -f '$host_key_path' '$host_pubkey_path'
                                  ssh-keygen -t ed25519 -N "" -f '$host_key_path'
                                fi
                                if [ ! -s '$host_pubkey_path' ]; then
                                  ssh-keygen -y -f '$host_key_path' > '$host_pubkey_path'
                                fi
                                chown root:root '$host_key_path' '$host_pubkey_path'
                                chmod 0600 '$host_key_path'
                                chmod 0644 '$host_pubkey_path'
                              "
                            }

                            retrieve() {
                              local host="$1"
                              local target
                              local dir
                              local out

                              require_host "$host"
                              target="$(ssh_target "$host")"
                              dir="$(host_workdir "$host")"
                              out="$dir/ssh_host_ed25519_key.pub"

                              mkdir -p "$dir"

                              if ! ssh "root@$target" "test -s '$host_pubkey_path' && cat '$host_pubkey_path'" > "$out"; then
                                rm -f "$out"
                                echo "Could not retrieve $host_pubkey_path from $host." >&2
                                return 42
                              fi

                              if [ ! -s "$out" ]; then
                                rm -f "$out"
                                echo "Retrieved public key from $host was empty." >&2
                                return 42
                              fi

                              printf '%s\n' "$out"
                            }

                            sign_key() {
                              local host="$1"
                              local pubkey="''${2:-$(host_workdir "$host")/ssh_host_ed25519_key.pub}"
                              local principals

                              require_host "$host"

                              if [ ! -f "$ca_key" ]; then
                                echo "Missing CA private key: $ca_key" >&2
                                exit 1
                              fi

                              if [ ! -f "$pubkey" ]; then
                                echo "Missing host public key: $pubkey" >&2
                                exit 1
                              fi

                              principals="$(principals_csv "$host")"
                              rm -f "''${pubkey%.pub}-cert.pub"

                              ssh-keygen \
                                -s "$ca_key" \
                                -I "$host" \
                                -h \
                                -n "$principals" \
                                -V "$validity" \
                                "$pubkey" >&2

                              printf '%s\n' "''${pubkey%.pub}-cert.pub"
                            }

                            deploy_cert() {
                              local host="$1"
                              local cert="''${2:-$(host_workdir "$host")/ssh_host_ed25519_key-cert.pub}"
                              local target

                              require_host "$host"

                              if [ ! -f "$cert" ]; then
                                echo "Missing host certificate: $cert" >&2
                                exit 1
                              fi

                              target="$(ssh_target "$host")"

                              ssh "root@$target" "
                                set -euo pipefail
                                umask 022
                                cat > '$host_cert_path'
                                chown root:root '$host_cert_path'
                                chmod 0644 '$host_cert_path'
                                systemctl reload sshd || systemctl restart sshd
                              " < "$cert"
                            }

                            rotate_one() {
                              local host="$1"
                              local bootstrap_mode="$2"
                              local pubkey
                              local cert

                              if ! pubkey="$(retrieve "$host")"; then
                                case "$bootstrap_mode" in
                                  always)
                                    echo "Bootstrapping $host because the ed25519 host public key is missing." >&2
                                    bootstrap "$host"
                                    pubkey="$(retrieve "$host")"
                                    ;;
                                  ask)
                                    if ask_yes_no "Bootstrap $host now?"; then
                                      bootstrap "$host"
                                      pubkey="$(retrieve "$host")"
                                    else
                                      echo "Skipping $host." >&2
                                      return 1
                                    fi
                                    ;;
                                  never)
                                    echo >&2
                                    echo "If the host key was deleted on the machine, run:" >&2
                                    echo "  mel-ssh-host-ca bootstrap $host" >&2
                                    echo "  mel-ssh-host-ca rotate $host" >&2
                                    echo >&2
                                    echo "Or do both in one step:" >&2
                                    echo "  mel-ssh-host-ca rotate --bootstrap $host" >&2
                                    return 1
                                    ;;
                                esac
                              fi

                              cert="$(sign_key "$host" "$pubkey")"
                              deploy_cert "$host" "$cert"
                            }

                            cmd="''${1:-}"
                            shift || true

                            case "$cmd" in
                              list)
                                names
                                ;;

                              principals)
                                host="''${1:-}"
                                [ -n "$host" ] || { usage >&2; exit 1; }
                                require_host "$host"
                                printf '%s\n' "$hosts_json" | jq -r --arg host "$host" '.[$host].principals[]'
                                ;;

                              bootstrap)
                                host="''${1:-}"
                                [ -n "$host" ] || { usage >&2; exit 1; }
                                bootstrap "$host"
                                ;;

                              retrieve)
                                host="''${1:-}"
                                [ -n "$host" ] || { usage >&2; exit 1; }
                                retrieve "$host"
                                ;;

                              sign)
                                host="''${1:-}"
                                pubkey="''${2:-}"
                                [ -n "$host" ] || { usage >&2; exit 1; }
                                if [ -n "$pubkey" ]; then
                                  sign_key "$host" "$pubkey"
                                else
                                  sign_key "$host"
                                fi
                                ;;

                              deploy)
                                host="''${1:-}"
                                cert="''${2:-}"
                                [ -n "$host" ] || { usage >&2; exit 1; }
                                if [ -n "$cert" ]; then
                                  deploy_cert "$host" "$cert"
                                else
                                  deploy_cert "$host"
                                fi
                                ;;

                              rotate)
                                bootstrap_mode="ask"
                                if [ "''${1:-}" = "--bootstrap" ]; then
                                  bootstrap_mode="always"
                                  shift
                                fi

                                host="''${1:-}"
                                [ -n "$host" ] || { usage >&2; exit 1; }
                                rotate_one "$host" "$bootstrap_mode"
                                ;;

                              rotate-all)
                                bootstrap_mode="ask"
                                failed=0
                                if [ "''${1:-}" = "--bootstrap" ]; then
                                  bootstrap_mode="always"
                                  shift
                                fi

                                for host in $(names); do
                                  echo "=== $host ==="
                                  if ! rotate_one "$host" "$bootstrap_mode"; then
                                    failed=1
                                  fi
                                done

                                exit "$failed"
                                ;;

                              -h|--help|help|"")
                                usage
                                ;;

                              *)
                                echo "Unknown command: $cmd" >&2
                                usage >&2
                                exit 1
                                ;;
                            esac
            '';
          };
      };

      apps.${system}.mel-ssh-host-ca = {
        type = "app";
        program = "${self.packages.${system}.mel-ssh-host-ca}/bin/mel-ssh-host-ca";
      };

      inherit nixosConfigurations;

      deploy.nodes = deployNodes;
    };
}
