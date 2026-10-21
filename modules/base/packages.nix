{ pkgs, ... }:
{
  environment.systemPackages = with pkgs; [
    aria2
    btop
    compsize
    cowsay
    dig
    git
    gnupg
    iperf3
    iptables
    jq
    libqalculate
    nfs-utils
    nftables
    python3
    ripgrep
    screen
    skopeo
    sl
    sshfs
    tcpdump
  ];
}
