{
  security = {
    audit = {
      enable = true;
      rules = [
        "-a always,exit -F arch=b64 -S execve -F euid=0 -F loginuid!=-1 -F key=root_cmd"
        "-a always,exit -F arch=b32 -S execve -F euid=0 -F loginuid!=-1 -F key=root_cmd"
      ];
    };
    auditd = {
      enable = true;
      settings.log_format = "ENRICHED";
    };
  };
}
