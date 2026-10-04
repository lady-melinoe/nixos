import QtQuick
import Quickshell.Io
import "../" as Bar

// waybar "custom/kb": not in style.css's shared module block, so it gets no
// background-color/padding override -- just the window's default text color.
Pill {
    id: root
    color: "transparent"
    hPad: 3
    clickable: true
    implicitWidth: label.implicitWidth + hPad * 2
    onClicked: proc.running = true

    Text {
        id: label
        anchors.centerIn: parent
        color: Bar.Colors.text
        font.pixelSize: 13
        font.family: "Symbols Nerd Font"
        text: "\uf11c"
    }

    Process {
        id: proc
        command: ["sh", "-c", "killall -s 34 wvkbd-mobintl"]
    }
}
