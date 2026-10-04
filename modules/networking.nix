{
  config,
  lib,
  ...
}:
let
  cfg = config.melinoe;
  melnodeCfg = config.melinoe.services.melnode;
  meshAddress = cfg.node.networking.intraIP;
in
{
  config = {
    assertions = [
      {
        assertion = melnodeCfg.enabled;
        message = "melinoe.services.melnode.enabled must be true for modules/networking.nix.";
      }
    ];

    networking.useDHCP = false;

    systemd.network = {
      enable = true;
      wait-online.timeout = 30;
      networks."10-lo" = {
        matchConfig.Name = "lo";
        address = [ "${meshAddress}/32" ];
        networkConfig.KeepConfiguration = "static";
      };
    };
  };
}
