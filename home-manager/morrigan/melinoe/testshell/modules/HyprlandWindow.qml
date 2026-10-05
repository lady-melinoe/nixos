import QtQuick
import Quickshell
import Quickshell.Io
import Quickshell.Hyprland

Item {
    id: root

    implicitWidth: label.implicitWidth + 27
    implicitHeight: 24

    // ─────────────────────────────────────────────
    // STATE (owned by us, not Hyprland directly)
    // ─────────────────────────────────────────────
    property var win: null

    Component.onCompleted: {

        Hyprland.rawEvent.connect(function(event) {

            if (event.name !== "activewindow" &&
                event.name !== "activewindowv2" &&
                event.name !== "closewindow" &&
                event.name !== "workspace")
                return

            const data = (event.data ?? "").toString().trim()

            // EMPTY STATE (this fixes stale window bug)
            if (event.name === "activewindowv2" && (!data || data === "")) {
                root.win = null
                return
            }

        if (event.name === "activewindow") {
            if (!data || data === "," || data === "") {
                root.win = null
                return
            }

            const [app = "", ...rest] = data.split(",")

            root.win = {
                app,
                title: rest.join(",")
            }

            return
        }

            if (event.name === "closewindow") {
                root.win = null
                return
            }

            if (event.name === "workspace") {
                // safety fallback: ensure empty workspace clears state
                if (!data || data === "" || data === "0") {
                    root.win = null
                }
            }
        })
    }

    // ─────────────────────────────────────────────
    // FORMATTING (clean switch-based logic)
    // ─────────────────────────────────────────────
    function windowLabel() {

        const w = root.win

        if (!w || (!w.app && !w.title))
            return "Trans Lives Matter <3"

        const app = w.app ?? ""
        const title = (w.title ?? "").replace(/^(.*?)( — .*?)?$/, "$1")

        let icon = ""

        switch (app) {

        case "librewolf":
            icon = ""
            break

        case "krita":
            icon = ""
            break

        case "kitty":
            icon = ""
            break

        case "com.mitchellh.ghostty":
            icon = ""
            break

        case "emacs":
            icon = ""
            break

        case "vesktop":
            icon = ""
            break

        case "GitKraken":
            icon = ""
            break

        case "Spotify":
            icon = ""
            break

        case "org.twosheds.iwgtk":
            icon = ""
            break

        default:
            // fallback: show something sensible
            if (title)
                return title
            if (app)
                return app
            return "Trans Lives Matter <3"
        }

        return icon + " " + title
    }

    // ─────────────────────────────────────────────
    // UI
    // ─────────────────────────────────────────────
    Text {
        id: label
        anchors.centerIn: parent

        color: "#F8F5FF"
        font.pixelSize: 13
        font.family: "Symbols Nerd Font"

        text: root.windowLabel()
        elide: Text.ElideRight
    }

    MouseArea {
        anchors.fill: parent
        onClicked: termToggle.running = true
    }

    Process {
        id: termToggle
        command: [
            "kitten", "@",
            "--to=unix:" + Quickshell.env("HOME") + "/.cache/term-panel",
            "resize-os-window",
            "--action=toggle-visibility"
        ]
    }
}
