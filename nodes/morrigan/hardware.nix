{ pkgs, ... }:
{
  boot.initrd.availableKernelModules = [
    "nvme"
    "xhci_pci"
    "thunderbolt"
    "usb_storage"
    "uas"
    "sd_mod"
  ];
  boot.kernelModules = [
    "kvm-amd"
    "i2c-dev"
    "hid-sony"
    "hid-playstation"
  ];

  hardware.enableRedistributableFirmware = true;
  hardware.cpu.amd.updateMicrocode = true;
  hardware.graphics = {
    enable = true;
    enable32Bit = true;
  };
  users.extraGroups.input.members = [ "melinoe" ];

  boot.loader = {
    grub.enable = false;
    systemd-boot.enable = false;
    efi.efiSysMountPoint = "/boot/efi";
  };
  boot.lanzaboote = {
    enable = true;
    pkiBundle = "/var/lib/sbctl";
  };
  environment.systemPackages = [ pkgs.sbctl ];

  services.tlp = {
    enable = true;
    settings = {
      STOP_CHARGE_THRESH_BAT0 = "1"; # Lenovo conservation mode
      DEVICES_TO_ENABLE_ON_STARTUP = "wifi bluetooth";
    };
  };
}
