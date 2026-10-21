{ lib }:
let
  inherit (lib) mkOption types;
in
{
  ip = import ./ip.nix { inherit lib; };

  nixFilesIn =
    dir:
    lib.filter (file: lib.hasSuffix ".nix" file && file != dir + "/default.nix") (
      lib.filesystem.listFilesRecursive dir
    );

  namedFilesIn =
    dir:
    lib.mapAttrs' (file: _: lib.nameValuePair (lib.removeSuffix ".nix" file) (dir + "/${file}")) (
      lib.filterAttrs (file: type: type == "regular" && lib.hasSuffix ".nix" file) (builtins.readDir dir)
    );

  mkAccessRuleOption =
    description:
    mkOption {
      inherit description;
      default = { };
      type = types.submodule {
        options = {
          tcp = mkOption {
            type = types.listOf types.port;
            default = [ ];
          };
          udp = mkOption {
            type = types.listOf types.port;
            default = [ ];
          };
          ipProtocols = mkOption {
            type = types.listOf types.ints.u8;
            default = [ ];
            description = "IP protocol numbers, e.g. 4 for IPIP.";
          };
        };
      };
    };

  afterMeshAddress =
    let
      unit = "systemd-networkd-wait-online@lo.service";
    in
    {
      after = [ unit ];
      wants = [ unit ];
    };
}
