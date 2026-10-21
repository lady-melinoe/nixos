{ pkgs, ... }:
{
  programs.quickshell = {
    enable = true;
    configs.bar = ./qml;
    activeConfig = "bar";
    systemd.enable = true;
  };

  home.packages = with pkgs; [
    iw
    impala
    bluetuith
    pulsemixer
  ];
}
