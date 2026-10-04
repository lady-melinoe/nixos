{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.melinoe;
  melnodeCfg = config.melinoe.services.melnode;
  addr = config.melinoe.cluster.networking;
  nodeID = cfg.node.id;

  pow2 = n: if n == 0 then 1 else 2 * pow2 (n - 1);
  mod = a: b: a - (a / b) * b;

  ip4ToInt = ip: lib.foldl' (acc: octet: acc * 256 + lib.toInt octet) 0 (lib.splitString "." ip);

  int4ToIp =
    n:
    lib.concatStringsSep "." (
      map (shift: toString (mod (n / pow2 shift) 256)) [
        24
        16
        8
        0
      ]
    );

  parseCidr =
    cidr:
    let
      parts = lib.splitString "/" cidr;
      ipInt = ip4ToInt (lib.elemAt parts 0);
      prefixLength = lib.toInt (lib.elemAt parts 1);
      hostBits = 32 - prefixLength;
      blockSize = pow2 hostBits;
      networkInt = ipInt - (mod ipInt blockSize);
    in
    if networkInt != ipInt then
      throw "melinoe.cluster.networking: ${cidr} is not aligned to its /${toString prefixLength} network base (did you mean ${int4ToIp networkInt}/${toString prefixLength}?)"
    else
      {
        inherit prefixLength hostBits blockSize;
        network = networkInt;
      };

  nodeAddress =
    cidr: id:
    let
      net = parseCidr cidr;
    in
    if id < 0 || id >= net.blockSize then
      throw "melinoe.cluster.networking: node id ${toString id} is out of range for ${cidr} (holds ${toString net.blockSize} addresses, 0-${toString (net.blockSize - 1)})"
    else
      int4ToIp (net.network + id);

  nodeIntraIP = nodeAddress addr.hostCidr;

  meshAddress = nodeIntraIP nodeID;
  meshAddressUnit = "melinoe-mesh-address.service";
in
{
  config = {
    assertions = [
      {
        assertion = melnodeCfg.enabled;
        message = "melinoe.services.melnode.enabled must be true for modules/networking.nix.";
      }
    ];

    _module.args.melinoeNodeIntraIP = nodeIntraIP;
    _module.args.melinoeAfterMeshAddress = {
      after = [ meshAddressUnit ];
      wants = [ meshAddressUnit ];
    };

    networking.useDHCP = false;

    systemd.network = {
      enable = true;
      networks."10-lo" = {
        matchConfig.Name = "lo";
        address = [ "${meshAddress}/32" ];
        linkConfig.RequiredForOnline = "no";
        networkConfig.KeepConfiguration = "static";
      };
    };

    systemd.services.melinoe-mesh-address = {
      description = "Wait for the mesh address on lo";
      after = [ "systemd-networkd.service" ];
      wants = [ "systemd-networkd.service" ];
      path = [ pkgs.iproute2 ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        TimeoutStartSec = 30;
      };
      script = ''
        until [ -n "$(ip -4 -o addr show dev lo to ${meshAddress}/32)" ]; do
          sleep 0.1
        done
      '';
    };
  };
}
