{
  melinoe.cluster.vms = {
    npm = {
      ip = "198.18.1.1";
      hostAccess.tcp = [ 8002 ];
    };
    npmalt = {
      ip = "198.18.1.2";
      hostAccess.tcp = [ 8002 ];
    };
    vaultwarden.ip = "198.18.1.3";
    authentik.ip = "198.18.1.4";
    homepage = {
      ip = "198.18.1.5";
      hostAccess.tcp = [ 61208 ];
    };
    mailserver = {
      ip = "198.18.1.6";
      tcp = [
        25
        465
      ];
    };
    radicale.ip = "198.18.1.7";
    "1710pack" = {
      ip = "198.18.1.8";
      tcp = [ 25565 ];
      udp = [ 25565 ];
    };
    website.ip = "198.18.1.9";
    mmmanager.ip = "198.18.1.10";
    drasl = {
      ip = "198.18.1.11";
      tcp = [ 57843 ];
      udp = [ 57843 ];
    };
    calibre.ip = "198.18.1.12";
    gitlab = {
      ip = "198.18.1.13";
      outboundDropIP = [ "35.190.167.255" ];
    };
    snappymail.ip = "198.18.1.14";
    dovecot = {
      ip = "198.18.1.15";
      tcp = [ 993 ];
    };
    stash = {
      ip = "198.18.1.16";
      outboundVia = "phaesyle";
    };
    ocis.ip = "198.18.1.17";
    collabora.ip = "198.18.1.18";
    ociscollab.ip = "198.18.1.19";
    play = {
      ip = "198.18.1.20";
      tcp = [ 8080 ];
      udp = [ 8080 ];
      outboundVia = "phaesyle";
    };
    devicebridge = {
      ip = "198.18.3.0/24";
      udp = [ 51820 ];
    };
    dadbridge = {
      ip = "198.18.4.0/24";
      udp = [ 51821 ];
    };
    glrunner0.ip = "198.18.2.0";
    mail2.ip = "198.18.1.21";
  };
}
