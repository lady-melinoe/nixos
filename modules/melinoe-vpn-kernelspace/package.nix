{
  lib,
  stdenv,
  kernel,
  kernelModuleMakeFlags,
}:
stdenv.mkDerivation {
  pname = "melnode-kernel";
  version = "0.0.1";

  src = ./src;

  hardeningDisable = [
    "pic"
    "format"
  ];

  nativeBuildInputs = kernel.moduleBuildDependencies;

  KDIR = "${kernel.dev}/lib/modules/${kernel.modDirVersion}/build";
  KERNELRELEASE = kernel.modDirVersion;

  kernelMakeFlags = kernelModuleMakeFlags;

  buildPhase = ''
    runHook preBuild
    make -C "$KDIR" M="$(pwd)" $kernelMakeFlags KERNELRELEASE="$KERNELRELEASE" modules
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    make -C "$KDIR" M="$(pwd)" $kernelMakeFlags KERNELRELEASE="$KERNELRELEASE" modules_install INSTALL_MOD_PATH="$out"
    runHook postInstall
  '';

  meta = with lib; {
    description = "melnode data plane as a Linux kernel module (genl family \"melnode\")";
    license = licenses.gpl2Only;
    platforms = platforms.linux;
  };
}
