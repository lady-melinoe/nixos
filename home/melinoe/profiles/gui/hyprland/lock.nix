{ lib, pkgs, ... }:
let
  splash = ../../../../../assets/splash.png;

  playerctl = lib.getExe pkgs.playerctl;
  hyprlock = lib.getExe pkgs.hyprlock;
  loginctl = "${pkgs.systemd}/bin/loginctl";
  lockCommand = "${playerctl} -a pause & ${hyprlock} --grace 0 --immediate-render --no-fade-in";

  fprintState = "\${XDG_RUNTIME_DIR:-/tmp}/hyprlock-fprint-state";
  fprintLock = "\${XDG_RUNTIME_DIR:-/tmp}/hyprlock-fprint-lock";

  fprintBind = pkgs.writeShellScriptBin "hyprlock-fprint-bind" ''
    state="${fprintState}"
    exec 9>"${fprintLock}"
    ${pkgs.util-linux}/bin/flock -n 9 || exit 0
    trap 'rm -f "$state"; pkill -x -USR2 hyprlock' EXIT
    echo waiting > "$state"
    pkill -x -USR2 hyprlock
    if timeout 5 fprintd-verify melinoe; then
      pkill -x -USR1 hyprlock
    else
      echo failed > "$state"
      pkill -x -USR2 hyprlock
      sleep 1
    fi
  '';

  fprintLabel = pkgs.writeShellScript "hyprlock-fprint-label" ''
    case "$(cat "${fprintState}" 2>/dev/null)" in
      waiting) echo '<span foreground="#33ccff">Touch the fingerprint sensor</span>' ;;
      failed) echo '<span foreground="#ff3355">Fingerprint failed</span>' ;;
      *) echo 'Use fingerprint' ;;
    esac
  '';

  label =
    extra:
    {
      monitor = "";
      color = "rgba(200, 200, 200, 1.0)";
      font_size = 16;
      font_family = "Noto Sans";
      halign = "center";
      valign = "center";
    }
    // extra;
in
{
  home.packages = [ fprintBind ];

  services.hypridle = {
    enable = true;
    settings = {
      general = {
        lock_cmd = lockCommand;
        before_sleep_cmd = "${lockCommand} && sleep 1";
        ignore_dbus_inhibit = false;
        ignore_systemd_inhibit = false;
      };
      listener = [
        {
          timeout = 600;
          on-timeout = "${loginctl} lock-session";
        }
      ];
    };
  };

  programs.hyprlock = {
    enable = true;
    settings = {
      general = {
        ignore_empty_input = true;
        immediate_render = true;
        fail_timeout = 1000;
      };
      background = [
        {
          monitor = "";
          path = "${splash}";
          color = "rgba(0, 0, 0, 1.0)";
          blur_passes = 2;
        }
      ];
      shape = [
        {
          monitor = "";
          size = "360, 60";
          color = "rgba(0, 0, 0, 0.0)";
          rounding = -1;
          border_size = 4;
          border_color = "rgba(0, 207, 230, 1.0)";
          position = "0, 80";
          halign = "center";
          valign = "center";
        }
      ];
      input-field = [
        {
          monitor = "";
          size = "20%, 5%";
          outline_thickness = 3;
          inner_color = "rgba(0, 0, 0, 0.0)";
          outer_color = "rgba(33ccffee) rgba(00ff99ee) 45deg";
          check_color = "rgba(00ff99ee) rgba(ff6633ee) 120deg";
          fail_color = "rgba(ff6633ee) rgba(ff0066ee) 40deg";
          font_color = "rgb(143, 143, 143)";
          fade_on_empty = false;
          rounding = 15;
          position = "0, -20";
          halign = "center";
          valign = "center";
        }
      ];
      label = [
        (label {
          text = "Hi there, $USER";
          font_size = 25;
          position = "0, 80";
        })
        {
          monitor = "";
          text = ''cmd[update:10000] echo -n "$(upower -i /org/freedesktop/UPower/devices/battery_BAT0 | grep percentage | grep -Po "\d+\%")           $TIME12"'';
          color = "rgba(10, 10, 10, 1.0)";
          font_size = 16;
          font_family = "Noto Sans";
          position = "-40, 14";
          halign = "right";
          valign = "bottom";
        }
        (label {
          text = "cmd[update:0:true] ${fprintLabel}";
          position = "0, -100";
          onclick = lib.getExe fprintBind;
        })
      ];
      auth.pam = {
        enabled = true;
        module = "hyprlock";
      };
      auth.fingerprint.enabled = false;
    };
  };
}
