//@ pragma UseQApplication
import QtQuick
import QtQuick.Layouts
import Quickshell
import "modules" as Modules
import "." as Bar

ShellRoot {
    PanelWindow {
        id: panel
        screen: Quickshell.screens.find(s => s.name === "eDP-1") ?? Quickshell.screens[0]

        anchors {
            left: true
            right: true
            bottom: true
        }

        implicitHeight: 36
        color: Bar.Colors.bg

        RowLayout {
            anchors.left: parent.left
            anchors.leftMargin: 3
            anchors.verticalCenter: parent.verticalCenter
            spacing: 4

            Modules.KeyboardToggle {}
            Modules.Network {}
            Modules.Bluetooth {}
            Modules.Pulseaudio {}
            Modules.Backlight {}
        }

        Modules.HyprlandWindow {
            anchors.centerIn: parent
        }

        RowLayout {
            anchors.right: parent.right
            anchors.rightMargin: 3
            anchors.verticalCenter: parent.verticalCenter
            spacing: 4

            Modules.Tray { panelWindow: panel }
            Modules.Temperature {}
            Modules.Cpu {}
            Modules.Battery {}
            Modules.Clock {}
            Modules.PowerMenu {}
        }
    }
}
