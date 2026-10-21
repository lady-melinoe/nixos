{
  melinoe.cluster.ssh = {
    userCA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINwS8hXHMP2ij1HmUL0N7oFU4+G8atQHtSRq9e8MOqkL SSH User CA";
    hostCA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPbv4PWCmELT4XxevCL+k8RnjrwgOfULXGgWQsVJUg9T";
    hostPatterns = [
      "*.infra.melinoe.xyz"
      "*.intra.melinoe.xyz"
      "198.18.0.*"
      "198.19.0.*"
      "198.19.1.*"
    ];
    builderHostPatterns = [
      "*.infra.melinoe.xyz"
      "198.18.0.*"
      "198.19.0.*"
      "198.19.1.*"
    ];
  };
}
