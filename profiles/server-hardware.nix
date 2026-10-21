{
  boot.initrd.availableKernelModules = [
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
  boot.initrd.kernelModules = [
    "nvme"
    "kvm-intel"
  ];
}
