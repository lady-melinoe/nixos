{ ... }:
{
  programs.bash.shellAliases = {
    dispense = "ssh -o loglevel=quiet -t motsugo dispense";
    ucc-adduser = ''ssh -o LogLevel=QUIET -J "melinoe@ssh.ucc.asn.au -o LogLevel=QUIET" -o SetEnv=TERM=xterm-256color -t melinoe@samson.ucc.asn.au sudo ucc-adduser'';
  };

  # Inlined rather than sourced from the store: the file exports PROMPT_COMMAND,
  # which defines `sshp` and carries itself to sudo / remote shells verbatim,
  # so it must stay byte-identical to the plain-text ~/.prompt on void.
  programs.bash.initExtra = builtins.readFile ./prompt.sh;
}
