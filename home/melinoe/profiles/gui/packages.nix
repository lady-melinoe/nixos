{ pkgs, ... }:
let
  # IPMIView with legacy BMC/KVM crypto compatibility: old Supermicro BMCs
  # only speak algorithms the bundled JRE disables by default.
  ipmiview = pkgs.ipmiview.overrideAttrs (old: {
    postPatch = (old.postPatch or "") + ''
      cat >> jre/conf/security/java.security <<'EOF'
      jdk.certpath.disabledAlgorithms=
      jdk.jar.disabledAlgorithms=
      jdk.tls.disabledAlgorithms=
      EOF
    '';
  });
in
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
    steam
    ipmiview
  ];
}
