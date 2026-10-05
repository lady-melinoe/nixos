import QtQuick
import Quickshell
import Quickshell.Io
import "../" as Bar

Pill {
    id: root
    color: Bar.Colors.paleViolet
    implicitWidth: label.implicitWidth + hPad * 2

    // Pure 0-100 system state
    property int brightnessPct: 0
    
    readonly property var icons: ["", "", "", "", "", "", "", "", ""]
    readonly property string icon: icons[Math.min(icons.length - 1, Math.max(0, Math.floor(brightnessPct / 100 * icons.length)))]

    Text {
        id: label
        anchors.centerIn: parent
        color: Bar.Colors.darkText
        font.pixelSize: 13
        font.family: "Symbols Nerd Font"
        text: root.brightnessPct + "% " + root.icon 
    }

    // 1. Initial fetch on startup
    Component.onCompleted: initialProc.running = true
    Process {
        id: initialProc
        command: ["brightnessctl", "-m"]
        stdout: StdioCollector {
            onStreamFinished: {
                const fields = text.trim().split(",")
                if (fields.length >= 4) root.brightnessPct = parseInt(fields[3])
                udevWatcher.running = true 
            }
        }
    }

    // 2. Direct Udev Monitor Watcher
    Process {
        id: udevWatcher
        command: ["udevadm", "monitor", "--subsystem=backlight"]
        stdout: SplitParser {
            splitMarker: "\n"
            onRead: (line) => {
                updateProc.running = true
            }
        }
    }

    Process {
        id: updateProc
        command: ["brightnessctl", "-m"]
        stdout: StdioCollector {
            onStreamFinished: {
                const fields = text.trim().split(",")
                if (fields.length >= 4) {
                    root.brightnessPct = parseInt(fields[3])
                }
            }
        }
    }

    // 3. Dampened scrolling control inputs
    MouseArea {
        anchors.fill: parent
        acceptedButtons: Qt.NoButton
        onWheel: (event) => {
            // angleDelta.y is typically +/- 120 per click on mice, but varies widely on trackpads.
            // Dividing by 240 cuts standard mouse ticks to 0.5% steps, heavily reducing trackpad spikes.
            let delta = event.angleDelta.y / 120
            
            // Ensure we move by at least 1% if there's intentional movement
            if (delta > 0 && delta < 1) delta = 1
            if (delta < 0 && delta > -1) delta = -1
            
            let nextPct = Math.max(0, Math.min(100, root.brightnessPct + Math.round(delta)))
            
            if (nextPct !== root.brightnessPct) {
                root.brightnessPct = nextPct
                
                scrollProc.command = ["brightnessctl", "set", root.brightnessPct + "%"]
                scrollProc.running = true
            }
        }
    }
    
    Process { id: scrollProc }
}
