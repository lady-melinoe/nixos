{ pkgs, ... }:
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
    # nix.gc only supports age-based cutoffs, which nukes everything if a
    # machine sits untouched for a while. Keep the last 5 system generations
    # regardless of age instead, then collect whatever's left unreferenced.
    gc.automatic = false;
  };

  systemd.services.nix-gc-keep-last = {
    description = "Trim old Nix system generations and collect garbage";
    serviceConfig.Type = "oneshot";
    path = [ pkgs.nix ];
    script = ''
      nix-env -p /nix/var/nix/profiles/system --delete-generations +5
      nix-collect-garbage
    '';
  };

  systemd.timers.nix-gc-keep-last = {
    wantedBy = [ "timers.target" ];
    timerConfig = {
      OnCalendar = "daily";
      Persistent = true;
    };
  };

  programs.nix-ld.enable = true;
  programs.appimage = {
    enable = true;
    binfmt = true;
  };
}
