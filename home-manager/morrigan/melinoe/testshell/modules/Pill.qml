import QtQuick

// Generic module "pill" background. Rectangle already exposes "data" as its
// default property, so children declared inside Pill{ ... } are placed
// directly into it -- just add your own Row/Text/Image as children.
Rectangle {
    id: root
    property bool clickable: false
    property real hPad: 7
    signal clicked()

    radius: 0
    implicitHeight: 36

    MouseArea {
        anchors.fill: parent
        enabled: root.clickable
        cursorShape: root.clickable ? Qt.PointingHandCursor : Qt.ArrowCursor
        onClicked: root.clicked()
    }
}
