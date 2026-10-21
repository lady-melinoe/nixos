{ config, lib, ... }:
let
  ssh = config.melinoe.cluster.ssh;
in
{
  services.openssh = {
    enable = true;
    settings = {
      PasswordAuthentication = false;
      KbdInteractiveAuthentication = false;
      TrustedUserCAKeys = "/etc/ssh/ssh-user-ca.pub";
      HostCertificate = ssh.hostCertPath;
    };
    extraConfig = "AcceptEnv PROFILEUSER";
  };

  melinoe.firewall.openPorts.tcp = config.services.openssh.ports;

  environment.etc = {
    "ssh/ssh-user-ca.pub" = {
      text = ssh.userCA;
      mode = "0644";
    };
    "ssh/ssh-host-ca.pub" = {
      text = "${ssh.hostCA} SSH Host CA";
      mode = "0644";
    };
    "ssh/ssh_known_hosts".text = ''
      @cert-authority ${lib.concatStringsSep "," ssh.hostPatterns} ${ssh.hostCA}
    '';
  };
}
