{
  pkgs,
  lib,
  nixosConfigurations,
}:
let
  stripCidr = address: lib.head (lib.splitString "/" address);

  hosts = lib.mapAttrs (
    name: host:
    let
      c = host.config;
      uplinks = c.melinoe.uplinks;
      buildMachines = lib.filter (m: m.sshKey != null) c.nix.buildMachines;
    in
    {
      hostname = c.networking.hostName;
      sshTarget = "${name}.infra.melinoe.xyz";
      hostCertificate = c.services.openssh.settings.HostCertificate;

      remoteBuildKeys = lib.unique (map (m: m.sshKey) buildMachines);
      remoteBuildKeyUsers = lib.mapAttrs (_: machines: lib.unique (map (m: m.sshUser) machines)) (
        lib.groupBy (m: m.sshKey) buildMachines
      );

      principals = lib.unique (
        [
          c.networking.hostName
          "${c.networking.hostName}.intra.melinoe.xyz"
          "${c.networking.hostName}.infra.melinoe.xyz"
        ]
        ++ lib.concatMap (
          uplink:
          lib.optional (uplink.address != null) (stripCidr uplink.address)
          ++ lib.optional (uplink.publicIp != null) uplink.publicIp
        ) uplinks
        ++ [ c.melinoe.node.address ]
      );
    }
  ) nixosConfigurations;

  package = lib.fileset.toSource {
    root = ./.;
    fileset = ./mel_ssh_provision;
  };
in
pkgs.writeShellApplication {
  name = "mel-ssh-provision";

  runtimeInputs = [
    pkgs.openssh
    pkgs.python3
  ];

  meta.description = "Melinoe SSH host provisioning tool";

  text = ''
    export MEL_HOSTS_JSON_PATH=${pkgs.writeText "mel-ssh-provision-hosts.json" (builtins.toJSON hosts)}
    export PYTHONPATH=${package}
    exec python3 -m mel_ssh_provision "$@"
  '';
}
