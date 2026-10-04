import QtQuick
import Quickshell
import "../" as Bar

// waybar "clock": format "{:%I:%M %p}", bg #AC70FF, text #2A2A2E
// click toggles alt format "{:%Y-%m-%d}"
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

    Text {
        id: label
        anchors.centerIn: parent
        color: Bar.Colors.darkText
        font.pixelSize: 13
        font.family: "Symbols Nerd Font"
        text: root.altFormat
            ? Qt.formatDate(root.now, "yyyy-MM-dd")
            : Qt.formatTime(root.now, "hh:mm AP")
    }

    // HoverHandler (rather than a second MouseArea) doesn't take an
    // exclusive grab, so it can't block the Pill's own click handling --
    // that's what made clicking "find a gap" before.
    HoverHandler {
        id: hover
    }

    PopupWindow {
        id: tip
        visible: hover.hovered
        anchor.item: root
        anchor.edges: Edges.Top
        anchor.gravity: Edges.Top
        implicitWidth: tipLabel.implicitWidth + 10
        implicitHeight: tipLabel.implicitHeight + 6

        Rectangle {
            anchors.fill: parent
            color: Bar.Colors.bg
            border.color: Bar.Colors.lavender
            border.width: 1
            Text {
                id: tipLabel
                anchors.centerIn: parent
                color: Bar.Colors.text
                font.pixelSize: 13
                text: Qt.formatDate(root.now, "dd MMMM yyyy")
            }
        }
    }
}
