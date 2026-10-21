import QtQuick
import Quickshell
import "../" as Bar

Pill {
    id: root
    color: Bar.Colors.lavender
    clickable: true
    implicitWidth: label.implicitWidth + hPad * 2

    property bool altFormat: false
    property date now: new Date()

    onClicked: altFormat = !altFormat

    Timer {
        interval: 1000
        running: true
        repeat: true
        onTriggered: root.now = new Date()
    }

    PillText {
        id: label
        text: root.altFormat
            ? Qt.formatDate(root.now, "yyyy-MM-dd")
            : Qt.formatTime(root.now, "hh:mm AP")
    }

    HoverHandler { id: hover }

    Tooltip {
        target: root
        shown: hover.hovered
        text: Qt.formatDate(root.now, "dd MMMM yyyy")
    }
}
