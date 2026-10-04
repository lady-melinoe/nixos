import QtQuick
import Quickshell.Io
import "../" as Bar

// waybar "cpu": format "{usage}% ", bg #AC70FF, text #2A2A2E
Pill {
    id: root
    color: Bar.Colors.lavender
    implicitWidth: label.implicitWidth + hPad * 2

    property real usage: 0
    property var prevIdle: 0
    property var prevTotal: 0

    Text {
        id: label
        anchors.centerIn: parent
        color: Bar.Colors.darkText
        font.pixelSize: 13
        text: Math.round(root.usage) + "% \uf2db"
    }

    Timer {
        interval: 2000
        running: true
        repeat: true
        triggeredOnStart: true
        onTriggered: proc.running = true
    }

    Process {
        id: proc
        command: ["sh", "-c", "head -n1 /proc/stat"]
        stdout: StdioCollector {
            onStreamFinished: {
                // cpu  user nice system idle iowait irq softirq steal
                const parts = text.trim().split(/\s+/).slice(1).map(Number)
                const idle = parts[3] + parts[4]
                const total = parts.reduce((a, b) => a + b, 0)
                const dIdle = idle - root.prevIdle
                const dTotal = total - root.prevTotal
                if (dTotal > 0)
                    root.usage = (1 - dIdle / dTotal) * 100
                root.prevIdle = idle
                root.prevTotal = total
            }
        }
    }
}
