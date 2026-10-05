{ ... }:
{
  programs.keychain = {
    enable = true;
    keys = [ ];
    extraFlags = [
      "--absolute"
      "--dir"
      "$XDG_RUNTIME_DIR/keychain"
      "--quiet"
    ];
    enableXsessionIntegration = false;
  };
}
