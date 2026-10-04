import QtQuick
import Quickshell
import Quickshell.Io
import Quickshell.Bluetooth
import "../" as Bar

Pill {
    id: root

    color: root.powered ? Bar.Colors.lavender : Bar.Colors.fuchsia
    clickable: true
    implicitWidth: label.implicitWidth + hPad * 2

    onClicked: launchProc.running = true

    readonly property var adapter: Bluetooth.defaultAdapter

    readonly property bool powered: adapter ? adapter.enabled : false
    readonly property string adapterName: adapter ? adapter.name : ""
    // BluetoothAdapter has no MAC/address property in this API version, and
    // this kernel doesn't expose it via /sys/class/bluetooth/<id>/address
    // either, so it's fetched once via bluetoothctl instead.
    property string adapterMac: ""

    onAdapterChanged: if (adapter) adapterMacProc.running = true
    Component.onCompleted: if (adapter) adapterMacProc.running = true

    Process {
        id: adapterMacProc
        command: ["bluetoothctl", "show"]

        stdout: StdioCollector {
            onStreamFinished: {
                const match = text.match(/Controller\s+([0-9A-Fa-f:]{17})/)
                if (match)
                    root.adapterMac = match[1]
            }
        }
    }

    readonly property var connectedDevices: adapter
        ? adapter.devices.values
            .filter(dev => dev.connected)
            .map(dev => ({
                name: dev.name,
                mac: dev.address,
                battery: dev.batteryAvailable
                    ? Math.round(dev.battery * 100).toString()
                    : ""
            }))
        : []

    property int currentDeviceIdx: 0
    property int scrollDelta: 0

    readonly property var activeDevice:
        connectedDevices.length > 0
            ? connectedDevices[currentDeviceIdx % connectedDevices.length]
            : null

    readonly property string connectedDevice:
        activeDevice ? activeDevice.name : ""

    readonly property string connectedBattery:
        activeDevice ? activeDevice.battery : ""

    property string tooltipText: {
        if (!root.powered)
            return "Bluetooth: Powered Off"

        let lines = []

        lines.push(
            root.adapterName
                ? root.adapterName + "  " + root.adapterMac
                : "Bluetooth Adapter  " + root.adapterMac
        )

        for (let i = 0; i < root.connectedDevices.length; i++) {
            const dev = root.connectedDevices[i]

            let line = "   " + dev.name + "  " + dev.mac

            if (dev.battery)
                line += "  " + dev.battery + "%"

            lines.push(line)
        }

        return lines.join("\n")
    }

    Text {
        id: label
        anchors.centerIn: parent
        color: Bar.Colors.darkText
        font.pixelSize: 13
        font.family: "Symbols Nerd Font"

        text: {
            if (!root.powered)
                return "\uf294 Off"

            if (root.connectedDevice.length)
                return "\uf294 " + root.connectedDevice +
                    (root.connectedBattery.length ? " " + root.connectedBattery + "%" : "")

            return "\uf294 On"
        }
    }

    onConnectedDevicesChanged: {
        // Reset the carousel index if the device count changed, mirroring
        // the previous behaviour when the device list was resynced.
        if (root.currentDeviceIdx >= root.connectedDevices.length)
            root.currentDeviceIdx = 0
    }

    MouseArea {
        anchors.fill: parent
        acceptedButtons: Qt.NoButton

        onWheel: (event) => {
            if (root.connectedDevices.length <= 1)
                return

            root.scrollDelta += event.angleDelta.y

            if (Math.abs(root.scrollDelta) >= 120) {
                let direction = root.scrollDelta > 0 ? 1 : -1
                let nextIdx = root.currentDeviceIdx - direction

                root.currentDeviceIdx =
                    nextIdx < 0
                        ? root.connectedDevices.length - 1
                        : nextIdx % root.connectedDevices.length

                root.scrollDelta = 0
            }
        }
    }

    Process {
        id: launchProc
        command: ["ghostty", "--command=bluetuith"]
    }
}
