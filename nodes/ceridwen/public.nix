{
  melinoe.nodePublicInfo."3" = {
    wgPubkey = "J1BWw4e4/jwXqYoajGiQZwHqsQxBM+Y079gy46PYzGs=";
    defaultEndpoint = "130.95.13.235";
    buildServer = {
      hostName = "ceridwen.infra.melinoe.xyz";
      publicHostCA = "@cert-authority *.infra.melinoe.xyz,198.18.0.*,198.19.0.*,198.19.1.*, ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
      supportedFeatures = [
        "nixos-test"
        "big-parallel"
        "kvm"
      ];
    };
  };
}
