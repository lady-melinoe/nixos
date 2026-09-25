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

  # Plain derivation attributes (Nix exports every string attr as an
  # environment variable) - NOT makeFlags, which only becomes the $makeFlags
  # bash array that genericBuild's *default* phases consume. buildPhase and
  # installPhase below call make directly, so anything only in makeFlags
  # would silently never reach make (which is exactly what happened before:
  # $KDIR was unset, so `make -C "$KDIR"` ran as `make -C ""` and failed).
  KDIR = "${kernel.dev}/lib/modules/${kernel.modDirVersion}/build";
  KERNELRELEASE = kernel.modDirVersion;

  # kernel.makeFlags (as opposed to kernelModuleMakeFlags) includes
  # "O=$(buildRoot)", a make-syntax reference to an env var that only exists
  # while building the kernel itself (see nixpkgs
  # pkgs/os-specific/linux/kernel/build.nix); make chokes on it here with
  # "empty variable name". kernelModuleMakeFlags is nixpkgs' own flag set for
  # exactly this case (see v4l2loopback's derivation for precedent): the same
  # toolchain (ARCH/CC/LD/...) without that landmine.
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
