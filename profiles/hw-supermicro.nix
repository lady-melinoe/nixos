{ lib, pkgs, ... }:
{
  boot.initrd.availableKernelModules = [
    "ahci"
    "ohci_pci"
    "ehci_pci"
    "pata_atiixp"
    "mpt3sas"
    "usb_storage"
    "usbhid"
    "sd_mod"
  ];
  boot.kernelModules = [ "kvm-amd" ];
  hardware.cpu.amd.updateMicrocode = true;

  nixpkgs.config.allowUnfreePredicate = pkg: builtins.elem (lib.getName pkg) [ "ipmicfg" ];

  environment.systemPackages = with pkgs; [
    ipmicfg
    ipmitool
    ipmiutil
  ];
}
