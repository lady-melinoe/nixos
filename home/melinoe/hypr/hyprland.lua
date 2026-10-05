-- hyprland.lua
-- Converted from hyprland.conf (hyprlang → Lua, Hyprland 0.55+)
-- Refer to: https://wiki.hypr.land/Configuring/Start/

-- ──────────────────────────────────────────────
-- AUTOSTART
-- ──────────────────────────────────────────────
hl.on("hyprland.start", function()
    hl.dsp.exec_cmd("loginctl lock-session")
end)

-- ──────────────────────────────────────────────
-- MONITORS
-- ──────────────────────────────────────────────
hl.monitor({
    output                = "eDP-1",
    mode                  = "highrr",
    position              = "auto",
    scale                 = 1.5,
    bitdepth              = 10,
    vrr                   = 0,
    cm                    = "hdredid",
    sdrbrightness         = 1,
    sdrsaturation         = 1,
    supports_wide_color   = true,
    supports_hdr          = true,
    sdr_min_luminance     = 0,
    sdr_max_luminance     = 300,
    min_luminance         = 0,
    max_luminance         = 570,
    max_avg_luminance     = 375,
})

-- ──────────────────────────────────────────────
-- SOURCED FILES  (replaces `source =`)
-- ──────────────────────────────────────────────
require("hyprland-de-setup")
require("hyprland-binds")

-- ──────────────────────────────────────────────
-- WINDOW RULES
-- ──────────────────────────────────────────────
hl.window_rule({
    match   = { class = "^(Firefox)$" },
    opacity = "1 override 0.9 override 1.0 override",
})
hl.window_rule({
    match = { class = "^(Firefox)$", title = "^$" },
    float = true,
})
hl.window_rule({
    match = { title = "^(Open File)$" },
    float = true,
})
hl.window_rule({
    match = { class = "^(xdg-desktop-portal-gtk)$" },
    float = true,
})
hl.window_rule({
    match = { class = "^(net-sourceforge-jnlp-runtime-Boot)$" },
    float = true,
})
-- Suppress maximize / fullscreen / fullscreenoutput events globally
hl.window_rule({
    match            = { class = ".*" },
    suppress_event  = "maximize fullscreen fullscreenoutput",
})

-- Prevent phantom XWayland float windows from stealing focus
hl.window_rule({
    match      = { class = "^$", title = "^$", xwayland = true,
                   float = true, fullscreen = false, pin = false },
    no_focus   = true,
})
-- Remove border/rounding on internally-fullscreen windows
-- NOTE: `fullscreen` = 1 targets the Hyprland-internal fullscreen state
hl.window_rule({
    match       = { fullscreen = 1 },
    border_size = 0,
    rounding    = 0,
})

-- ──────────────────────────────────────────────
-- WORKSPACE RULES
-- ──────────────────────────────────────────────
-- f[1] = first fullscreen workspace: no gaps
hl.workspace_rule({ workspace = "f[1]", gaps_in = 0, gaps_out = 0 })

-- ──────────────────────────────────────────────
-- APPEARANCE
-- ──────────────────────────────────────────────
hl.config({
    general = {
        gaps_in         = 5,
        gaps_out        = { top = "10", right = "10", bottom = "5", left = "10"},
        border_size     = 2,
        col = {
            active_border   = { colors = {"rgba(33ccffee)", "rgba(00ff99ee)"}, angle = 45 },
            inactive_border = "rgba(595959aa)",
        },
        resize_on_border = false,
        allow_tearing    = true,
        layout           = "dwindle",
    },

    decoration = {
        rounding         = 10,
        active_opacity   = 1.0,
        inactive_opacity = 1.0,
        shadow = {
            enabled      = false,
            range        = 4,
            render_power = 3,
            color        = "rgba(1a1a1aee)",
        },
        blur = {
            enabled = false,
        },
    },

    dwindle = {
        preserve_split = true,
    },

    master = {
        new_status = "master",
    },

    xwayland = {
        force_zero_scaling = true,
    },

    render = {
        direct_scanout = 0,
        cm_auto_hdr    = 1,
    },

    misc = {
        force_default_wallpaper = 2,
        disable_hyprland_logo   = true,
    },
})

-- Animations (separate hl.animation / hl.bezier calls are also valid)
hl.animation = {
    leaf = "global",
    enabled = true,
    bezier  = "myBezier, 0.05, 0.9, 0.1, 1.05",
    speed = "5"
}

