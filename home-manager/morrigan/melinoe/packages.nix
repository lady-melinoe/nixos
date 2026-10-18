{ pkgs, ... }:
{
  home.packages = with pkgs; [
    librewolf
    rofi
    xournalpp
    gnome-disk-utility
    thunderbird
    krita
    fluffychat
    qpwgraph
    signal-desktop
    calibre
    mpv
    spotify
    jdk21
    vesktop
    heroic
    lutris
  ];
}
