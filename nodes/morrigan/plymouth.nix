{ lib, pkgs, ... }:
let
  assets = ../../common/assets;

  frames = lib.sort lib.lessThan (
    lib.filter (name: lib.hasPrefix "load" name && lib.hasSuffix ".png" name) (
      builtins.attrNames (builtins.readDir assets)
    )
  );

  startupTheme = pkgs.runCommand "plymouth-theme-startup" { } ''
    dir=$out/share/plymouth/themes/startup
    mkdir -p $dir

    cp ${assets}/splash.png $dir/background.png

    i=0
    for frame in ${lib.escapeShellArgs (map (name: "${assets}/${name}") frames)}; do
      cp $frame $dir/frame$i.png
      i=$((i + 1))
    done

    sed 's/FRAME_COUNT/${toString (builtins.length frames)}/g' ${./plymouth/startup.script} > $dir/startup.script

    cat > $dir/startup.plymouth <<PLYMOUTH
    [Plymouth Theme]
    Name=startup
    ModuleName=script

    [script]
    ImageDir=$dir
    ScriptFile=$dir/startup.script
    PLYMOUTH
  '';
in
{
  boot = {
    plymouth = {
      enable = true;
      theme = "startup";
      themePackages = [ startupTheme ];
    };
    initrd = {
      systemd.enable = true;
      verbose = false;
      kernelModules = [ "amdgpu" ];
    };
    consoleLogLevel = 0;
    kernelParams = [
      "quiet"
      "splash"
    ];
  };

  systemd.services.plymouth-quit.after = [
    "hypr-session.service"
    "tty-login.service"
  ];
}
