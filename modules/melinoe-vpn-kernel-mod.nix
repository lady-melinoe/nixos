{
  config,
  lib,
  pkgs,
  ...
}:
let
  inherit (lib) mkOption mkIf types;
  cfg = config.melinoe.services.melnode.kernelDataplane;

  melnodeKernelPackage = config.boot.kernelPackages.callPackage ./melinoe-vpn-kernel/melnode-kernel.nix { };
in
{
  # This option (and the package above) only get evaluated/built when
  # kernelDataplane.enable is set, which is what keeps a normal, all-userspace
  # host from ever pulling the GPLv2 kmod source into its closure.
  options.melinoe.services.melnode.kernelDataplane = {
    enable = mkOption {
      type = types.bool;
      default = false;
      description = ''
        Load the melnode kernel module (modules/melinoe-vpn-kernel) instead of
        running melnode-dp in userspace. GPLv2, built out-of-tree against
        `config.boot.kernelPackages` for this host.

        Currently a skeleton: it loads and unloads cleanly but does not yet
        register the "melnode" genl family, so melnode-cp still needs
        melnode-dp. Do not enable this on a real node yet.
      '';
    };

    package = mkOption {
      type = types.package;
      default = melnodeKernelPackage;
      readOnly = true;
      description = "The built melnode.ko kernel module package.";
    };
  };

  config = mkIf cfg.enable {
    boot.extraModulePackages = [ cfg.package ];
    boot.kernelModules = [ "melnode" ];
  };
}
