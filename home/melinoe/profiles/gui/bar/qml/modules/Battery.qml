import QtQuick
import Quickshell.Services.UPower
import "../" as Bar

Pill {
    id: root
    clickable: true
    onClicked: altFormat = !altFormat

    property bool altFormat: false

    readonly property var dev: UPower.displayDevice
    readonly property bool present: dev && dev.isLaptopBattery
    readonly property real pct: present ? dev.percentage * 100 : 0
    readonly property bool charging: present &&
        (dev.state === UPowerDeviceState.Charging || dev.state === UPowerDeviceState.PendingCharge)
    readonly property bool critical: present && pct <= 15 && !charging
    readonly property int secsRemaining: present
        ? (charging ? dev.timeToFull : dev.timeToEmpty)
        : 0

    function formatTime(secs) {
        if (!secs || secs <= 0) return "…"
        const h = Math.floor(secs / 3600)
        const m = Math.floor((secs % 3600) / 60)
        return h + "h " + m + "m"
    }

    visible: present
    implicitWidth: present ? label.implicitWidth + hPad * 2 : 0

    color: {
        if (!present) return "transparent"
        if (charging) return Bar.Colors.teal
        if (critical) return blink.on ? Bar.Colors.fuchsia : Bar.Colors.text
        return Bar.Colors.paleViolet
    }

    QtObject { id: blink; property bool on: true }
    Timer {
        interval: 500
        running: root.critical
        repeat: true
        onTriggered: blink.on = !blink.on
    }

    PillText {
        id: label
        text: root.altFormat
            ? root.formatTime(root.secsRemaining) + " " + root.icon
            : Math.round(root.pct) + "% " + root.icon
    }

    readonly property string icon: {
        if (charging) return "\uf0e7"
        if (pct > 90) return "\uf240"
        if (pct > 60) return "\uf241"
        if (pct > 30) return "\uf242"
        if (pct > 10) return "\uf243"
        return "\uf244"
    }
}
