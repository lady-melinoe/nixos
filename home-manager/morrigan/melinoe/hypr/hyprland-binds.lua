-- hyprland-binds.lua
-- Converted from hyprland-binds.conf (hyprlang → Lua, Hyprland 0.55+)

-- ──────────────────────────────────────────────
-- INPUT CONFIGURATION
-- ──────────────────────────────────────────────
hl.config({
    input = {
        kb_layout    = "us",
        kb_variant   = "",
        kb_model     = "",
        kb_options   = "",
        kb_rules     = "",
        follow_mouse = 1,
        sensitivity  = 1.0,
        touchpad = {
            natural_scroll          = false,
            scroll_factor           = 1.0,
            middle_button_emulation = false,
            clickfinger_behavior    = false,
            tap_to_click            = true,
            disable_while_typing    = false,
            drag_3fg                = 1,
            tap_and_drag            = false,
        },
        touchdevice = {},
    },
})

-- ──────────────────────────────────────────────
-- KEYBINDS
-- ──────────────────────────────────────────────
local mainMod = "SUPER"

-- Core Application Launchers & Session Control

hl.bind(mainMod .. " + Q",     hl.dsp.exec_cmd("ghostty"))
hl.bind(mainMod .. " + B",     hl.dsp.exec_cmd("gtk-launch librewolf"))
hl.bind(mainMod .. " + R",     hl.dsp.exec_cmd("killall rofi || rofi -show drun"))
hl.bind(mainMod .. " + space", hl.dsp.exec_cmd("killall rofi || rofi -show drun"))
hl.bind(mainMod .. " + C",     hl.dsp.window.close())
hl.bind(mainMod .. " + L",     hl.dsp.exec_cmd("loginctl lock-session"))
hl.bind(mainMod .. " + M",     hl.dsp.exec_cmd("hyprctl reload"))
hl.bind(mainMod .. " + SHIFT + M", hl.dsp.exit(), { locked = true, release = true })

-- Utilities & Toggles
hl.bind(mainMod .. " + ALT + S", hl.dsp.exec_cmd([[printf "\u00A7" | wl-copy -]]))
hl.bind(mainMod .. " + a",       hl.dsp.exec_cmd("killall activate-linux || activate-linux"))
hl.bind(mainMod .. " + SHIFT + R", hl.dsp.exec_cmd("killall iio-hyprland || iio-hyprland"))
hl.bind(mainMod .. " + K",       hl.dsp.exec_cmd("killall -s 34 wvkbd-mobintl"))
hl.bind(mainMod .. " + t",       hl.dsp.exec_cmd("kitten @ --to=unix:${HOME}/.cache/term-panel resize-os-window --action=toggle-visibility"))

hl.bind("Right", hl.dsp.exec_cmd("pgrep -x hyprlock && hyprlock-fprint-bind"), {
    locked = true,
    release = true,
    non_consuming = true,
    transparent = true,
})

-- Media & Hardware Controls
hl.bind("XF86AudioNext",  hl.dsp.exec_cmd("playerctl next"), { locked = true })
hl.bind("XF86AudioPause", hl.dsp.exec_cmd("playerctl play-pause"), { locked = true })
hl.bind("XF86AudioPlay",  hl.dsp.exec_cmd("playerctl play-pause"), { locked = true })
hl.bind("XF86AudioPrev",  hl.dsp.exec_cmd("playerctl previous"), { locked = true })

hl.bind(mainMod .. " + XF86AudioRaiseVolume", hl.dsp.exec_cmd("brightnessctl s 5%+"), { repeating = true, locked = true })
hl.bind(mainMod .. " + XF86AudioLowerVolume", hl.dsp.exec_cmd("brightnessctl s 5%-"), { repeating = true, locked = true })
hl.bind("XF86AudioRaiseVolume",  hl.dsp.exec_cmd("wpctl set-volume @DEFAULT_AUDIO_SINK@ 5%+ -l 1.2"), { repeating = true, locked = true })
hl.bind("XF86AudioLowerVolume",  hl.dsp.exec_cmd("wpctl set-volume @DEFAULT_AUDIO_SINK@ 5%- -l 1.2"), { repeating = true, locked = true })
hl.bind("XF86AudioMute",         hl.dsp.exec_cmd("wpctl set-mute @DEFAULT_AUDIO_SINK@ toggle"), { repeating = true, locked = true })
hl.bind("XF86AudioMicMute",      hl.dsp.exec_cmd("wpctl set-mute @DEFAULT_AUDIO_SOURCE@ toggle"), { repeating = true, locked = true })
hl.bind("XF86MonBrightnessUp",   hl.dsp.exec_cmd("brightnessctl s 5%+"), { repeating = true, locked = true })
hl.bind("XF86MonBrightnessDown", hl.dsp.exec_cmd("brightnessctl s 5%-"), { repeating = true, locked = true })

-- Window State & Operations
hl.bind(mainMod .. " + F",         hl.dsp.window.fullscreen({ mode = "fullscreen", action = "toggle" }))
hl.bind(mainMod .. " + SHIFT + F", hl.dsp.window.fullscreen({ mode = "maximized", action = "toggle" }))
hl.bind(mainMod .. " + CTRL + F",  hl.dsp.window.fullscreen_state({ internal = "-1", client = "2", action = "toggle" }))
hl.bind(mainMod .. " + V",         hl.dsp.window.float({ action = "toggle" }))
hl.bind(mainMod .. " + P",         hl.dsp.window.pseudo({ action = "toggle" }))

-- Focus & Window Movement
hl.bind(mainMod .. " + left",  hl.dsp.focus({ direction = "l" }))
hl.bind(mainMod .. " + right", hl.dsp.focus({ direction = "r" }))
hl.bind(mainMod .. " + up",    hl.dsp.focus({ direction = "u" }))
hl.bind(mainMod .. " + down",  hl.dsp.focus({ direction = "d" }))

hl.bind(mainMod .. " + SHIFT + left",  hl.dsp.window.move({ direction = "l" }))
hl.bind(mainMod .. " + SHIFT + right", hl.dsp.window.move({ direction = "r" }))
hl.bind(mainMod .. " + SHIFT + up",    hl.dsp.window.move({ direction = "u" }))
hl.bind(mainMod .. " + SHIFT + down",  hl.dsp.window.move({ direction = "d" }))

-- Mouse Binds (Scroll & Drag)
hl.bind(mainMod .. " + mouse_down", hl.dsp.focus({ workspace = "e+1" }))
hl.bind(mainMod .. " + mouse_up",   hl.dsp.focus({ workspace = "e-1" }))

hl.bind(mainMod .. " + mouse:272",         hl.dsp.window.drag(),   { mouse = true })
hl.bind(mainMod .. " + SHIFT + mouse:272", hl.dsp.window.resize(), { mouse = true })
hl.bind(mainMod .. " + mouse:273",         hl.dsp.window.resize(), { mouse = true })

-- ──────────────────────────────────────────────
-- WORKSPACE CONTROLS
-- ──────────────────────────────────────────────
for i = 1, 10 do
    local key = tostring(i % 10)
    hl.bind(mainMod .. " + " .. key, hl.dsp.focus({ workspace = i }))
    hl.bind(mainMod .. " + SHIFT + " .. key, hl.dsp.window.move({ workspace = i, follow = false }))
    hl.bind(mainMod .. " + ALT + " .. key, function()
        hl.dispatch(hl.dsp.focus({ workspace = i }))
        hl.dispatch(hl.dsp.focus({ monitor = "-1" }))
    end)
end
