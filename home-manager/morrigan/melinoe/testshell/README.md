# Waybar → Quickshell QML port

Direct port of your waybar `config.jsonc` + `style.css` + `power_menu.xml`,
scaled by ÷1.5 throughout (bar height 36→24, base font 13→9, module padding
10→7, spacing 4→3, tray icon-size 14→9).

## Layout
- `shell.qml` — `PanelWindow` anchored `left/right/bottom` on output `eDP-1`
  (waybar: `layer: top`, `position: bottom`, `output: ["eDP-1"]`, `height: 36`).
- `modules/` — one file per waybar module, named after the waybar module id:
  - `KeyboardToggle.qml` ← `custom/kb`
  - `Network.qml` ← `network` (nmcli-polled, wifi/ethernet/disconnected states)
  - `Bluetooth.qml` ← `bluetooth` (bluetoothctl-polled)
  - `Pulseaudio.qml` ← `pulseaudio` (native `Quickshell.Services.Pipewire`,
    scroll = volume step, click launches `pulsemixer`)
  - `Backlight.qml` ← `backlight` (brightnessctl, scroll = ±5%)
  - `HyprlandWindow.qml` ← `hyprland/window`, with every regex from your
    `rewrite` table ported 1:1 to JS `.replace()`/`.match()`, including the
    "Trans Lives Matter <3" easter egg and the dropdown-terminal `on-click`.
  - `Tray.qml` ← `tray` (native `Quickshell.Services.SystemTray`)
  - `Temperature.qml` ← `temperature` (reads a thermal zone directly —
    **edit the path in `Temperature.qml`** to match your sensor, same as you'd
    edit waybar's `hwmon-path`/`thermal-zone`)
  - `Cpu.qml` ← `cpu` (delta of `/proc/stat`)
  - `Battery.qml` ← `battery` (native `Quickshell.Services.UPower`, including
    the critical blink and charging/plugged teal)
  - `Clock.qml` ← `clock` (click toggles date format, tooltip shows month)
  - `PowerMenu.qml` ← `custom/power` + `power_menu.xml`, rendered as a real
    `PopupWindow` menu with the same items/order/separators (Suspend,
    Hibernate, Shutdown, ―, Reboot, ―, Log Out) wired to the same
    `menu-actions` commands.
- `Colors.qml` — palette pulled straight from `style.css` (deep purple bg
  `#240844`, lavender `#AC70FF`, pale violet `#C68AFF`, teal `#52FFBD`,
  fuchsia `#FF4AD9`, peach `#FFA769`).

Not ported (they were commented out in your config): `custom/touchtoggle`,
`custom/nag`, `custom/term`, `mpd`, `keyboard-state`. Easy to add back from
the corresponding waybar block if you turn them on later — the `Pill.qml`
base component + a `Process`/`Timer` pair is the pattern used everywhere
here.

## Requirements
- Quickshell (obviously), built with the Hyprland, Pipewire, UPower, and
  SystemTray services enabled.
- `brightnessctl`, `nmcli`, `bluetoothctl`, `bluetuith`, `pulsemixer`,
  `iwgtk`, `kitty`, `kitten` — same external tools your waybar config already
  shells out to.
- **A Font Awesome (or Nerd Font) font installed**, same as waybar's
  `otf-font-awesome` requirement. GTK CSS silently falls back through your
  `font-family` list; QML `Text` does not — if icons render as boxes, set
  `font.family: "FontAwesome"` (or your installed Nerd Font name) on the
  `Text` elements in each module, or set it once as an application-wide
  default font in `shell.qml`.

## Try it
```sh
qs -p /path/to/quickshell-waybar
```
(or however you normally launch your Quickshell config — adjust to a
config-dir install under `~/.config/quickshell/<name>/` if you use named
configs).

## Known rough edges to sanity-check on your machine
- `Temperature.qml`'s thermal zone path is a guess — point it at the right
  `/sys/class/thermal/thermal_zoneN/temp` or `hwmon` path for your CPU.
- `Network.qml`/`Bluetooth.qml` poll via shelling out rather than a native
  Quickshell service (Quickshell doesn't ship NetworkManager/bluez services
  out of the box the way it does Pipewire/UPower) — functionally equivalent
  to waybar's own approach, just written explicitly instead of hidden inside
  a C++ module.
- `PowerMenu.qml`'s popup positioning uses `PopupWindow`'s item-anchoring;
  double check it lands where you want given your bar's `bottom` position
  and screen setup.
