{
  config,
  lib,
  ...
}:
let
  inherit (lib) mkIf mkMerge mkDefault;
  nodeType = config.melinoe.node.type;
in
{
  config = mkMerge [
    (mkIf (nodeType == "server") {
      boot.initrd = {
        availableKernelModules = [
          "ata_piix"
          "uhci_hcd"
          "xen_blkfront"
          "vmw_pvscsi"
          "sd_mod"
          "usbhid"
          "usb_storage"
          "mpt3sas"
          "ehci_pci"
        ];
        kernelModules = [
          "nvme"
          "kvm-intel"
        ];
      };
    })
    (mkIf (nodeType == "workstation") {
      melinoe.node.isVMHost = mkDefault false;
      melinoe.node.isRemoteUpdatable = mkDefault false;
      melinoe.services.haproxy.enable = mkDefault false;
    })
  ];
}
