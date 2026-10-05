import QtQuick
import Quickshell
import Quickshell.Services.SystemTray
import Quickshell.Widgets
import "../" as Bar

Pill {
    id: root

    required property var panelWindow

    color: Bar.Colors.lavender
    implicitWidth: row.implicitWidth + hPad * 2

    Row {
        id: row
        anchors.centerIn: parent
        spacing: 7

        Repeater {
            model: SystemTray.items

            Item {
                id: trayIcon
                width: 18
                height: 18

                required property SystemTrayItem modelData

                IconImage {
                    anchors.fill: parent
                    source: modelData.icon
                }

                MouseArea {
                    anchors.fill: parent
                    acceptedButtons: Qt.LeftButton | Qt.RightButton

                    onClicked: (event) => {
                        if (event.button === Qt.RightButton && modelData.hasMenu) {
                            // Position menu relative to the tray icon.
                            let iconPos = trayIcon.mapToItem(root.panelWindow.contentItem, 0, 0)

                            let spawnX = iconPos.x
                            let spawnY = iconPos.y

                            modelData.display(root.panelWindow, spawnX, spawnY)
                        } else {
                            modelData.activate()
                        }
                    }
                }
            }
        }
    }
}
