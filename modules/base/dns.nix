{
  services.resolved = {
    enable = true;
    settings.Resolve.FallbackDns = [ ];
  };

  networking = {
    nameservers = [
      "9.9.9.11"
      "149.112.112.11"
      "9.9.9.9"
      "149.112.112.112"
      "1.1.1.1"
      "1.0.0.1"
    ];
    domain = "infra.melinoe.xyz";
    search = [
      "infra.melinoe.xyz"
      "intra.melinoe.xyz"
      "ucc.asn.au"
    ];
  };
}
