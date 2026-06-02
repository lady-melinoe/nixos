{ pkgs, ... }:

{
  users.users.melinoe = {
    isNormalUser = true;
    home = "/home/melinoe";
    description = "melinoe_wuz_here";
    extraGroups = [
      "wheel"
      "networkmanager"
    ];
    shell = pkgs.bash;
  };
}
