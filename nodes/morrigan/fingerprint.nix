{ pkgs, ... }:
let
  libfprint-goodix55b4 = pkgs.libfprint.overrideAttrs (old: {
    version = "1.94.6";

    src = pkgs.fetchFromGitHub {
      owner = "TheWeirdDev";
      repo = "libfprint";
      rev = "c1937b99ec3db5abca05f619a95d2e37496d8810";
      hash = "sha256-+TDZb4C/kF1nDEP/e/I1FXxC86KVSYo1jyAx6oAFkj0=";
    };

    patches = [
      ./fingerprint/0001-sigfm-make-sigfm-tests-conditional-on-doctest.patch
      ./fingerprint/0002-goodixtls55x4-accept-the-GF3268_RTSEC_APP-firmware-f.patch
      ./fingerprint/0003-goodixtls55x4-re-provision-the-PSK-instead-of-only-c.patch
      ./fingerprint/0004-goodixtls-fix-preset-PSK-write-framing.patch
      ./fingerprint/0005-goodixtls-enable-PSK-ciphers-for-the-sensor-TLS-hand.patch
      ./fingerprint/0006-sigfm-tighten-matching-thresholds-to-reduce-false-po.patch
    ];

    postPatch = ''
      patchShebangs \
        tests/unittest_inspector.py \
        tests/virtual-image.py \
        tests/umockdev-test.py \
        tests/test-generated-hwdb.sh
    '';
    nativeBuildInputs = old.nativeBuildInputs ++ [ (pkgs.python3.withPackages (p: [ p.pygobject3 ])) ];
    buildInputs = old.buildInputs ++ [ pkgs.opencv ];

    doInstallCheck = false;
  });
in
{
  services.fprintd = {
    enable = true;
    package = (pkgs.fprintd.override { libfprint = libfprint-goodix55b4; }).overrideAttrs (old: {
      postPatch = ''
        ${old.postPatch or ""}
        substituteInPlace meson.build --replace-fail "1.94.9" "1.94.6"
      '';
    });
  };
}
