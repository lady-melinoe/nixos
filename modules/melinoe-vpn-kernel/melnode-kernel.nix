{
  lib,
  stdenv,
  kernel,
  kernelModuleMakeFlags,
}:
let
  # melnode_genl.h is the wire contract between melnode-cp and any data
  # plane (see dpproto/API.md); the userspace copy lives under the main
  # project's license, this one under GPLv2, but the two must stay
  # byte-for-byte identical or the kernel and Go sides silently disagree
  # about command/attribute numbers. Nix asserts that here instead of
  # relying on a build-time diff, so it fails at eval time either way.
  ownHeader = ./src/melnode_genl.h;
  dpprotoHeader = ../melinoe-vpn-daemon-src/dpproto/melnode_genl.h;
in
assert lib.assertMsg (
  builtins.readFile ownHeader == builtins.readFile dpprotoHeader
) "melnode-kernel: ${toString ownHeader} has drifted from ${toString dpprotoHeader} - they must be kept byte-for-byte identical (copy the dpproto one over and re-check licensing notes at its top).";
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
