{
  lib,
  config,
  ...
}:
{
  config = {
    assertions = map (
      peer:
      let
        peerIdStr = toString peer.id;
        hasOverride = peer.endpoint != null;
        hasDefault =
          config.melinoe.nodePublicInfo ? ${peerIdStr}
          && config.melinoe.nodePublicInfo.${peerIdStr}.defaultEndpoint != null;
      in
      {
        assertion = hasOverride || hasDefault;
        message = "melinoe.node.networking.peers: Peer ID ${peerIdStr} has no endpoint override configured, and no defaultEndpoint is found in nodePublicInfo for this node.";
      }
    ) config.melinoe.node.networking.peers;
  };
}
