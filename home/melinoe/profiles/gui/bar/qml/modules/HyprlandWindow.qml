import QtQuick
import Quickshell
import Quickshell.Io
import Quickshell.Hyprland

Item {
    id: root

    implicitWidth: label.implicitWidth + 27
    implicitHeight: 24

    property var win: null

    readonly property var icons: ({
        "librewolf": "",
        "krita": "",
        "kitty": "",
        "com.mitchellh.ghostty": "",
        "emacs": "",
        "vesktop": "",
        "GitKraken": "",
        "Spotify": "",
        "org.twosheds.iwgtk": ""
    })

    readonly property string idle: "Trans Lives Matter <3"

    Component.onCompleted: {
        Hyprland.rawEvent.connect(event => {
            const data = (event.data ?? "").toString().trim()

            switch (event.name) {
            case "activewindowv2":
                if (!data)
                    root.win = null
                break
            case "activewindow":
                if (!data || data === ",") {
                    root.win = null
                } else {
                    const [app = "", ...title] = data.split(",")
                    root.win = { app, title: title.join(",") }
                }
                break
            case "closewindow":
                root.win = null
                break
            case "workspace":
                if (!data || data === "0")
                    root.win = null
                break
            }
        })
    }

    function windowLabel() {
        const w = root.win
        if (!w || (!w.app && !w.title))
            return idle

        const app = w.app ?? ""
        const title = (w.title ?? "").replace(/^(.*?)( — .*?)?$/, "$1")
        const icon = icons[app]

        if (icon === undefined)
            return title || app || idle
        return icon + " " + title
    }

    PillText {
        id: label
        color: "#F8F5FF"
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
