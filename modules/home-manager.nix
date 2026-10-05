{
  config,
  lib,
  inputs,
  ...
}:
let
  inherit (lib) mkIf mkOption types;
  cfg = config.melinoe.home-manager;
in
{
  imports = [ inputs.home-manager.nixosModules.home-manager ];

  options.melinoe.home-manager.melinoe = mkOption {
    type = types.listOf types.deferredModule;
    default = [ ];
    description = "Home Manager modules to apply to the melinoe user.";
  };

  config = mkIf (cfg.melinoe != [ ]) {
    home-manager = {
      useGlobalPkgs = true;
      useUserPackages = true;
      users.melinoe.imports = cfg.melinoe;
    };
  };
}
