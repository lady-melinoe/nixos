{ ... }:
{
  programs.bash.shellAliases = {
    dispense = "ssh -o loglevel=quiet -t motsugo dispense";
    ucc-adduser = ''ssh -o LogLevel=QUIET -J "melinoe@ssh.ucc.asn.au -o LogLevel=QUIET" -o SetEnv=TERM=xterm-256color -t melinoe@samson.ucc.asn.au sudo ucc-adduser'';
  };
}
