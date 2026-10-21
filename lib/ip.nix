{ lib }:
let
  toInt = ip: lib.foldl' (acc: octet: acc * 256 + lib.toInt octet) 0 (lib.splitString "." ip);

  fromInt =
    n:
    lib.concatMapStringsSep "." (unit: toString (lib.mod (n / unit) 256)) [
      16777216
      65536
      256
      1
    ];
in
{
  nodeAddress =
    cidr: id:
    let
      parts = lib.splitString "/" cidr;
      size = lib.foldl' (n: _: n * 2) 1 (lib.range 1 (32 - lib.toInt (lib.last parts)));
    in
    if id < 0 || id >= size then
      throw "node id ${toString id} does not fit in ${cidr} (${toString size} addresses)"
    else
      fromInt (toInt (lib.head parts) + id);
}
