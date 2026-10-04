-- hyprland-de-setup.lua
-- Converted from hyprland-de-setup.conf (hyprlang → Lua, Hyprland 0.55+)

-- ──────────────────────────────────────────────
-- ENVIRONMENT VARIABLES
-- ──────────────────────────────────────────────
-- NOTE: $scale was a hyprlang variable with no definition in the original
-- source files. Define it here as a Lua local and adjust to taste.
local scale = 1.5

hl.env("XCURSOR_SIZE",                    "36")
hl.env("HYPRCURSOR_THEME",                "Adwaita")
hl.env("HYPRCURSOR_SIZE",                 "24")

hl.env("QT_AUTO_SCREEN_SCALE_FACTOR",      "1")
hl.env("QT_QPA_PLATFORM",                 "wayland;xcb")
hl.env("QT_QPA_PLATFORMTHEME",            "qt6ct")
hl.env("QT_WAYLAND_DIABLE_WINDOWDECORATION", "1")


hl.env("GDK_SCALE",                       tostring(scale))
hl.env("GTK_THEME",                       "Dracula-dark")

-- ──────────────────────────────────────────────
-- AUTOSTART  (exec-once)
-- ──────────────────────────────────────────────
-- quickshell (testshell), pipewire, hyprpaper, hypridle, mako, hyprpolkitagent, xdg-desktop-portal-hyprland
-- and the cursor theme are managed by NixOS/home-manager (systemd), not started here.
hl.on("hyprland.start", function()
    hl.exec_cmd("rm -f ${HOME}/.cache/term-panel")
    hl.exec_cmd(
        "kitten panel --edge=bottom --lines=15 --focus-policy=on-demand " ..
        "--layer top -o background_opacity=1 --start-as-hidden " ..
        "--listen-on=unix:${HOME}/.cache/term-panel " ..
        "-o allow_remote_control=socket-only kitten run-shell"
    )
    hl.exec_cmd("wvkbd-mobintl -L 256 -H 512 --hidden")
    hl.exec_cmd("activate-linux")
    hl.exec_cmd("iio-hyprland")
end)

-- ──────────────────────────────────────────────
-- BAR / LAYER RULES
-- ──────────────────────────────────────────────
hl.layer_rule({ match = { namespace = "waybar" },       order      = 2     })
hl.layer_rule({ match = { namespace = "waybar" },       no_anim    = true  })
hl.layer_rule({ match = { namespace = "waybar" },       above_lock = true  })
hl.layer_rule({ match = { namespace = "kitty-panel" },  order      = 3     })
hl.layer_rule({ match = { namespace = "kitty-panel" },  no_anim    = true  })


hl.on("window.title", function(w)
    if w.title == "Extension: (Bitwarden Password Manager) - Bitwarden — LibreWolf" then
        local winSelector = { address = w.address }
        hl.dispatch(hl.dsp.window.float({ action = "set" }, winSelector))
        hl.dispatch(hl.dsp.window.center(winSelector))
    end
end)
