{
  pkgs,
  lib,
  nixosConfigurations,
}:
{
  mel-deploy = import ./mel-deploy { inherit pkgs lib nixosConfigurations; };
  mel-ssh-provision = import ./mel-ssh-provision { inherit pkgs lib nixosConfigurations; };
}
