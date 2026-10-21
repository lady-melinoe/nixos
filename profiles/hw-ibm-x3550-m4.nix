{
  boot.initrd.availableKernelModules = [
    "ehci_pci"
    "ahci"
    "mpt3sas"
    "sd_mod"
  ];
  boot.kernelModules = [ "kvm-intel" ];
  hardware.cpu.intel.updateMicrocode = true;
}
