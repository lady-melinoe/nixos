import QtQuick
import Quickshell.Io
import "../" as Bar

Pill {
    id: root
    color: root.tempC >= 80 ? Bar.Colors.fuchsia : Bar.Colors.lavender
    implicitWidth: label.implicitWidth + hPad * 2

    property real tempC: 0

    PillText {
        id: label
        text: Math.round(root.tempC) + "°C " + (root.tempC >= 80 ? "\uf2c7" : "\uf2c8")
    }

    Timer {
        interval: 5000
        running: true
        repeat: true
        triggeredOnStart: true
        onTriggered: proc.running = true
    }

    Process {
        id: proc
        command: ["sh", "-c", "cat /sys/class/thermal/thermal_zone0/temp 2>/dev/null || echo 0"]
        stdout: StdioCollector {
            onStreamFinished: {
                const milli = parseInt(text.trim())
                if (!isNaN(milli))
                    root.tempC = milli / 1000
            }
        }
    }
}
