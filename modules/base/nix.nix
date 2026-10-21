{
  nix = {
    settings = {
      experimental-features = [
        "nix-command"
        "flakes"
      ];
      min-free = 2147483648;
      max-free = 5368709120;
      require-sigs = true;
    };
    gc = {
      automatic = true;
      dates = "daily";
      options = "--delete-older-than 14d";
    };
  };

  programs.nix-ld.enable = true;
  programs.appimage = {
    enable = true;
    binfmt = true;
  };
}
