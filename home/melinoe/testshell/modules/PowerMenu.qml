import QtQuick
import Quickshell
import Quickshell.Io
import "../" as Bar

// waybar "custom/power": format "⏻ ", opens power_menu.xml on click.
// Items/order/labels/separators ported 1:1; actions from menu-actions.
Pill {
    id: root
    color: "transparent"
    hPad: 3
    clickable: true
    implicitWidth: label.implicitWidth + hPad * 2
    onClicked: menu.visible = !menu.visible

    Text {
        id: label
        anchors.centerIn: parent
        color: Bar.Colors.text
        font.pixelSize: 13
        font.family: "Symbols Nerd Font"
        text: "\uf011"
    }

    HoverHandler { id: buttonHover }

    function run(cmd) {
        menu.visible = false
        runner.command = ["sh", "-c", cmd]
        runner.running = true
    }
    Process { id: runner }

    // Close the menu shortly after the mouse leaves both the button and the
    // popup itself, with a small grace period so crossing the gap between
    // them (or a brief flicker) doesn't slam it shut.
    Timer {
        id: closeTimer
        interval: 250
        onTriggered: menu.visible = false
    }
    onVisibleMenuHoverChanged: {
        if (!buttonHover.hovered && !menuHover.hovered && menu.visible)
            closeTimer.restart()
        else
            closeTimer.stop()
    }
    property bool visibleMenuHover: buttonHover.hovered || menuHover.hovered
    property alias menuHovered: menuHover.hovered

    PopupWindow {
        id: menu
        visible: false
        anchor.item: root
        anchor.edges: Edges.Bottom | Edges.Right
        anchor.gravity: Edges.Bottom | Edges.Right
        implicitWidth: 100
        implicitHeight: col.implicitHeight

        HoverHandler { id: menuHover }

        Rectangle {
            anchors.fill: parent
            color: Bar.Colors.bg
            border.color: Bar.Colors.lavender
            border.width: 1

            Column {
                id: col
                width: parent.width

                component MenuItem: Rectangle {
                    property string text: ""
                    property string action: ""
                    width: parent.width
                    height: 20
                    color: ma.containsMouse ? Bar.Colors.lavender : "transparent"
                    Text {
                        anchors.left: parent.left
                        anchors.leftMargin: 7
                        anchors.verticalCenter: parent.verticalCenter
                        text: parent.text
                        color: ma.containsMouse ? Bar.Colors.darkText : Bar.Colors.text
                        font.pixelSize: 13
                    }
                    MouseArea {
                        id: ma
                        anchors.fill: parent
                        hoverEnabled: true
                        onClicked: root.run(parent.action)
                    }
                }

                MenuItem { text: "Suspend"; action: "loginctl lock-session; sleep 1; loginctl suspend" }
                MenuItem { text: "Hibernate"; action: "loginctl hibernate" }
                MenuItem { text: "Shutdown"; action: "loginctl poweroff" }
                Rectangle { width: parent.width; height: 1; color: Bar.Colors.lavender }
                MenuItem { text: "Reboot"; action: "loginctl reboot" }
                Rectangle { width: parent.width; height: 1; color: Bar.Colors.lavender }
                MenuItem { text: "Log Out"; action: "hyprctl dispatch exit" }
            }
        }
    }
}
