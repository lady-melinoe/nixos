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
            bottom: true // waybar "position": "bottom"
        }

        implicitHeight: 36
        color: Bar.Colors.bg

        RowLayout {
            anchors.left: parent.left
            anchors.leftMargin: 3
            anchors.verticalCenter: parent.verticalCenter
            spacing: 4

            Modules.KeyboardToggle {}

            // 1. Network with its Tooltip Overlay
            Modules.Network {
                id: netModule

                HoverHandler { id: netHover }

                PopupWindow {
                    visible: netHover.hovered
                    anchor.window: panel
                    anchor.item: netModule
                    anchor.edges: Edges.Top
                    anchor.gravity: Edges.Top
                    
                    // Fixed: Using implicit sizes as requested by Quickshell API
                    implicitWidth: netTooltipBody.width
                    implicitHeight: netTooltipBody.height

                    Rectangle {
                        id: netTooltipBody
                        color: "#1e1e2e" // Dark Catppuccin variant
                        border.color: Bar.Colors.lavender
                        border.width: 1
                        radius: 4
                        width: netText.implicitWidth + 12
                        height: netText.implicitHeight + 12

                        Text {
                            id: netText
                            x: 6
                            y: 6
                            color: "#cdd6f4"
                            font.pixelSize: 16
                            font.family: "Inter, Sans"
                            text: netModule.tooltipText
                        }
                    }
                }
            }

            // 2. Bluetooth with its Tooltip Overlay
            Modules.Bluetooth {
                id: btModule

                HoverHandler { id: btHover }

                PopupWindow {
                    visible: btHover.hovered
                    anchor.window: panel
                    anchor.item: btModule
                    anchor.edges: Edges.Top
                    anchor.gravity: Edges.Top
                    
                    // Fixed: Using implicit sizes as requested by Quickshell API
                    implicitWidth: btTooltipBody.width
                    implicitHeight: btTooltipBody.height

                    Rectangle {
                        id: btTooltipBody
                        color: "#1e1e2e"
                        border.color: Bar.Colors.lavender
                        border.width: 1
                        radius: 4
                        width: btText.implicitWidth + 12
                        height: btText.implicitHeight + 12

                        Text {
                            id: btText
                            x: 6
                            y: 6
                            color: "#cdd6f4"
                            font.pixelSize: 16
                            font.family: "Inter, Sans"
                            text: btModule.tooltipText
                        }
                    }
                }
            }
            
            Modules.Pulseaudio {}
            Modules.Backlight {}
        }

        // modules-center: hyprland/window
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
