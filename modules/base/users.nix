{ pkgs, ... }:
{
  users.users.melinoe = {
    isNormalUser = true;
    description = "melinoe_wuz_here";
    extraGroups = [
      "wheel"
      "networkmanager"
    ];
    shell = pkgs.bash;
    openssh.authorizedKeys.keys = [
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK4aBJqk5/c2gFqPTcK3II7733F5wGCdt1wEIjI2K9e5 melinoe@lilith"
    ];
  };

  programs.gnupg.agent = {
    enable = true;
    pinentryPackage = pkgs.pinentry-curses;
  };
}
