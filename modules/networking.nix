{
  config,
  lib,
  ...
}:
let
  cfg = config.melinoe;
  routeCfg = config.melinoe.node.networking.melinoe-route;
  addr = config.melinoe.cluster.networking;
  nodeID = cfg.node.id;

  pow2 = n: if n == 0 then 1 else 2 * pow2 (n - 1);
  mod = a: b: a - (a / b) * b;

  ip4ToInt =
    ip:
    lib.foldl' (acc: octet: acc * 256 + lib.toInt octet) 0 (lib.splitString "." ip);

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
  nodeWgIP = nodeAddress addr.wireguardCidr;
  nodeLoopbackIP = nodeAddress addr.bgpCidr;
in
{
  config = {
    assertions = [
      {
        assertion = routeCfg.enabled;
        message = "melinoe.node.networking.melinoe-route.enabled must be true for modules/networking.nix.";
      }
    ];

    _module.args.melinoeNodeIntraIP = nodeIntraIP;
    _module.args.melinoeNodeWgIP = nodeWgIP;
    _module.args.melinoeNodeLoopbackIP = nodeLoopbackIP;

    networking.useDHCP = false;
    networking.interfaces.lo.ipv4.addresses = [
      {
        address = nodeLoopbackIP nodeID;
        prefixLength = 32;
      }
      {
        address = nodeIntraIP nodeID;
        prefixLength = 32;
      }
    ];
  };
}
