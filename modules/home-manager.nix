{
  config,
  lib,
  inputs,
  ...
}:
let
  cfg = config.melinoe.home;
in
{
  imports = [ inputs.home-manager.nixosModules.home-manager ];

  options.melinoe.home = lib.mkOption {
    type = lib.types.listOf lib.types.deferredModule;
    default = [ ];
    description = "Home Manager modules for the melinoe user.";
  };

  config = lib.mkIf (cfg != [ ]) {
    home-manager = {
      useGlobalPkgs = true;
      useUserPackages = true;
      users.melinoe.imports = cfg;
    };
  };
}
