{ buildGoModule }:
buildGoModule {
  pname = "melnode";
  version = "0.0.2";

  src = ./src;
  subPackages = [
    "cmd/melnode-dp"
    "cmd/melnode-cp"
  ];
  vendorHash = "sha256-qG9O8ed6bK0WpaQxof+pxsMa7IudmB0rnGTWYjuXm2g=";
}
