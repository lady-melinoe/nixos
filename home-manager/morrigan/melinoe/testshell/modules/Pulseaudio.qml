import QtQuick
import Quickshell.Io
import Quickshell.Services.Pipewire
import "../" as Bar

// waybar "pulseaudio": format "{volume}% {icon} {format_source}"
// muted -> dark bg + pale-violet icon
Pill {
    id: root

    readonly property var sink: Pipewire.defaultAudioSink
    readonly property var source: Pipewire.defaultAudioSource
    readonly property real volume: sink && sink.audio ? sink.audio.volume : 0
    readonly property bool muted: sink && sink.audio ? sink.audio.muted : false
    readonly property real micVolume: source && source.audio ? source.audio.volume : 0
    readonly property bool micMuted: source && source.audio ? source.audio.muted : false

    color: muted ? Bar.Colors.darkText : Bar.Colors.lavender
    clickable: true
    implicitWidth: label.implicitWidth + hPad * 2
    onClicked: launchProc.running = true

    PwObjectTracker { objects: [root.sink, root.source] }

    Text {
        id: label
        anchors.centerIn: parent
        color: root.muted ? Bar.Colors.paleViolet : Bar.Colors.darkText
        font.pixelSize: 13
        font.family: "Symbols Nerd Font"
        text: (root.muted ? "\uf6a9" : Math.round(root.volume * 100) + "% " + root.icon)
            + (root.source
                ? "  " + (root.micMuted ? "\uf131" : "\uf130 " + Math.round(root.micVolume * 100) + "%")
                : "")
    }

    readonly property string icon: {
        if (volume > 0.6) return "\uf028"
        if (volume > 0) return "\uf027"
        return "\uf026"
    }

    MouseArea {
        anchors.fill: parent
        acceptedButtons: Qt.NoButton
        onWheel: (event) => {
            if (!root.sink || !root.sink.audio) return
            const step = 0.005 // 0.5%
            root.sink.audio.volume = Math.max(0, Math.min(1,
                root.sink.audio.volume + (event.angleDelta.y > 0 ? step : -step)))
        }
    }

    Process { id: launchProc; command: ["kitty", "pulsemixer"] }
}
