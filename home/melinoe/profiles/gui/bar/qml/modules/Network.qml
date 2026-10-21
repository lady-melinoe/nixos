import QtQuick
import Quickshell.Io
import "../" as Bar

Pill {
    id: root

    color: connected ? Bar.Colors.lavender : Bar.Colors.fuchsia
    clickable: true
    implicitWidth: label.implicitWidth + hPad * 2

    onClicked: launchProc.running = true

    property bool connected: false
    property string essid: ""
    property int signal: 0
    property string ip: ""
    property bool showIp: false
    property int scrollDelta: 0
    property string tooltipText: "Network Status"

    PillText {
        id: label

        text: {
            if (!root.connected)
                return "Disconnected "

            if (!root.essid)
                return "Connected 󰖩"

            if (root.showIp && root.ip.length)
                return root.ip + " 󰖩"

            return root.essid + " (" + root.signal + "%) 󰖩"
        }
    }

    MouseArea {
        anchors.fill: parent
        acceptedButtons: Qt.NoButton

        onWheel: (event) => {
            root.scrollDelta += event.angleDelta.y

            if (Math.abs(root.scrollDelta) >= 120) {
                root.showIp = !root.showIp
                root.scrollDelta = 0
            }
        }
    }

    HoverHandler { id: hover }

    Tooltip {
        target: root
        shown: hover.hovered
        text: root.tooltipText
        background: "#1e1e2e"
        foreground: "#cdd6f4"
        radius: 4
        pixelSize: 16
        fontFamily: "Inter, Sans"
        hPad: 6
        vPad: 6
    }

    Timer {
        interval: 3000
        running: true
        repeat: true
        triggeredOnStart: true
        onTriggered: proc.running = true
    }

    Process {
        id: proc

        command: [
            "sh", "-c",
            "iw dev wlan0 link; echo '###JSON###'; ip -j addr show scope global"
        ]

        stdout: StdioCollector {
            onStreamFinished: {
                const marker = "###JSON###";
                const parts = text.split(marker);

                const wifi = parts[0] || "";
                const jsonPart = parts[1] || "[]";

                root.connected = !wifi.includes("Not connected");

                const ssidMatch = wifi.match(/SSID:\s(.+)/);
                root.essid = ssidMatch ? ssidMatch[1].trim() : "";

                const signalMatch = wifi.match(/signal:\s(-?\d+)\s*dBm/);

                if (signalMatch) {
                    const dbm = parseInt(signalMatch[1]);
                    root.signal = Math.max(0, Math.min(100, 2 * (dbm + 100)));
                } else {
                    root.signal = 0;
                }

                try {
                    const ifaces = JSON.parse(jsonPart);

                    let blocks = [];
                    let primaryIp = "";

                    for (const iface of ifaces) {
                        if (iface.ifname === "lo")
                            continue;

                        let header = iface.ifname;

                        if (iface.ifname.startsWith("wlan") && root.connected) {
                            header += `  ${root.essid} (${root.signal}%)`;
                        }

                        let ipv4 = [];
                        let ipv6 = [];

                        for (const addr of (iface.addr_info || [])) {
                            if (addr.family === "inet") {
                                ipv4.push(addr.local);

                                if (iface.ifname === "wlan0" && !primaryIp) {
                                    primaryIp = addr.local;
                                }
                            }

                            if (addr.family === "inet6") {
                                ipv6.push(addr.local);
                            }
                        }

                        let block = header;

                        if (ipv4.length)
                            block += `\n    ${ipv4.join(", ")}`;

                        if (ipv6.length)
                            block += `\n    ${ipv6.join(", ")}`;

                        blocks.push(block);
                    }

                    root.ip = primaryIp;

                    root.tooltipText = blocks.join("\n");

                } catch (e) {
                    root.tooltipText = "Network: Error parsing IP data";
                }
            }
        }
    }

    Process {
        id: launchProc
        command: ["ghostty", "--command=impala"]
    }
}
