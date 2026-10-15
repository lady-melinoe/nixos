{
  melinoe.other.nodePublicInfo."4" = {
    wgPubkey = "41zfU8FrcEMtD4MHr2Aq8kj/1oujLpOExeCJe6lyUEE=";
    defaultEndpoint = "130.95.13.234";
    buildServer = {
      hostName = "benzaiten.infra.melinoe.xyz";
      publicHostCA = "@cert-authority *.infra.melinoe.xyz,198.18.0.*,198.19.0.*,198.19.1.*, ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T SSH Host CA";
      supportedFeatures = [
        "nixos-test"
        "big-parallel"
        "kvm"
      ];
    };
  };
}
