{
  environment.sessionVariables = {
    XDG_CACHE_HOME = "$HOME/.cache";
    XDG_CONFIG_HOME = "$HOME/.config";
    XDG_DATA_HOME = "$HOME/.local/share";
    XDG_STATE_HOME = "$HOME/.local/state";

    HISTFILE = "$HOME/.local/state/bash/history";
    LESSHISTFILE = "$HOME/.local/state/less/history";
    VTYSH_HISTFILE = "$HOME/.local/state/frr/history";

    PYTHON_HISTORY = "$HOME/.local/state/python/history";
    PYTHONHISTFILE = "$HOME/.local/state/python/history";
    PYTHONPYCACHEPREFIX = "$HOME/.cache/python";
    PYTHONUSERBASE = "$HOME/.local/share/python";

    CLAUDE_CONFIG_DIR = "$HOME/.config/claude";
    CODEX_HOME = "$HOME/.local/share/codex";
    GOPATH = "$HOME/.local/share/go";
    GOMODCACHE = "$HOME/.cache/go/mod";
    PERF_BUILDID_DIR = "$HOME/.cache/perf";
    PULSE_COOKIE = "$HOME/.config/pulse/cookie";
    WGETRC = "/etc/wgetrc";
  };

  environment.etc."wgetrc".text = ''
    hsts-file = ~/.cache/wget-hsts
  '';

  environment.extraInit = ''
    if [ -n "$XDG_RUNTIME_DIR" ]; then
      export SCREENDIR="$XDG_RUNTIME_DIR/screen"
    fi
  '';

  programs.bash.interactiveShellInit = ''
    mkdir -p "$XDG_STATE_HOME/bash" "$XDG_STATE_HOME/python" "$XDG_STATE_HOME/less" "$XDG_STATE_HOME/frr"
  '';
}
