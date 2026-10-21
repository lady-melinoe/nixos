import QtQuick
import Quickshell
import "../" as Bar

PopupWindow {
    id: root

    required property Item target
    property bool shown: false
    property alias text: label.text
    property color background: Bar.Colors.bg
    property color foreground: Bar.Colors.text
    property real radius: 0
    property real pixelSize: 13
    property string fontFamily: Qt.application.font.family
    property real hPad: 5
    property real vPad: 3

    visible: shown
    anchor.item: target
    anchor.edges: Edges.Top
    anchor.gravity: Edges.Top
    implicitWidth: label.implicitWidth + hPad * 2
    implicitHeight: label.implicitHeight + vPad * 2

    Rectangle {
        anchors.fill: parent
        color: root.background
        radius: root.radius
        border.color: Bar.Colors.lavender
        border.width: 1

        Text {
            id: label
            anchors.centerIn: parent
            color: root.foreground
            font.pixelSize: root.pixelSize
            font.family: root.fontFamily
        }
    }
}
