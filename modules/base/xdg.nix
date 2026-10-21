{
  environment.sessionVariables = {
    XDG_CACHE_HOME = "$HOME/.cache";
    XDG_CONFIG_HOME = "$HOME/.config";
    XDG_DATA_HOME = "$HOME/.local/share";
    XDG_STATE_HOME = "$HOME/.local/state";

    HISTFILE = "$HOME/.local/state/bash/history";

    PYTHON_HISTORY = "$HOME/.local/state/python/history";
    PYTHONHISTFILE = "$HOME/.local/state/python/history";
    PYTHONPYCACHEPREFIX = "$HOME/.cache/python";
    PYTHONUSERBASE = "$HOME/.local/share/python";
  };

  programs.bash.interactiveShellInit = ''
    mkdir -p "$XDG_STATE_HOME/bash" "$XDG_STATE_HOME/python"
  '';
}
