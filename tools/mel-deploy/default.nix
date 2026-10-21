{
  pkgs,
  lib,
  nixosConfigurations,
}:
let
  domain = "intra.melinoe.xyz";

  deployable = lib.filterAttrs (_: host: host.config.melinoe.update.remote) nixosConfigurations;

  fqdns = lib.concatStringsSep " " (
    lib.mapAttrsToList (name: _: "[${name}]=${lib.escapeShellArg "${name}.${domain}"}") deployable
  );
in
pkgs.writeShellApplication {
  name = "mel-deploy";

  runtimeInputs = with pkgs; [
    coreutils
    gnugrep
    gnused
    openssh
  ];

  meta.description = "Melinoe fleet deployment helper (parses [DEPLOY] commit tags)";

  text = ''
    declare -A fqdns=(${fqdns})
    ${builtins.readFile ./mel-deploy.sh}
  '';
}
