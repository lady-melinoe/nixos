import QtQuick
import Quickshell.Io
import "../" as Bar

Pill {
    id: root
    color: "transparent"
    hPad: 3
    clickable: true
    implicitWidth: label.implicitWidth + hPad * 2
    onClicked: proc.running = true

    PillText {
        id: label
        color: Bar.Colors.text
        text: "\uf11c"
    }

    Process {
        id: proc
        command: ["sh", "-c", "killall -s 34 wvkbd-mobintl"]
    }
}
