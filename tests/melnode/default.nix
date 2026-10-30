# VM test for melnode: the real module, data plane and kernel (the pinned one) on three nodes in a
# line, and what has to survive a crash.
#
#   nix build .#checks.x86_64-linux.melnode -L
{
  inputs,
  pkgs,
  lib,
  melinoeLib,
}:
let
  inherit (import ./nodes.nix { inherit lib; }) nodes node;
in
pkgs.testers.runNixOSTest {
  name = "melnode";

  # boot.nix overlays nixpkgs for the pinned kernel, which a read-only pkgs forbids.
  node.pkgsReadOnly = false;
  node.specialArgs = { inherit inputs melinoeLib; };
  nodes = lib.mapAttrs (name: _: {
    imports = [ (node name) ];
    virtualisation.memorySize = 1536;
  }) nodes;

  testScript = ''
    import json

    nodes = [a, b, c]

    def links(m):
        return json.loads(m.succeed("curl -sf http://127.0.0.1:60198/links"))

    def services_up(m):
        m.wait_for_unit("melnode-cp.service")
        m.wait_for_unit("melnode-helper.service")

    def dpctl(m, args, prefix=""):
        """melnode-dpctl against the data plane the running (or last) melnode-cp configured."""
        show = "systemctl show -p ExecStart --value melnode-cp.service"
        exe = m.succeed(f"{show} | grep -o '/nix/store/[^ ;]*/bin/melnode-cp' | head -1").strip()
        conf = m.succeed(f"{show} | grep -o '/nix/store/[^ ;]*melnode-cp.toml' | head -1").strip()
        obj = m.succeed(f"sed -n 's/^ebpfObject = \"\\(.*\\)\"$/\\1/p' {conf}").strip()
        return m.succeed(f"{prefix}env MELNODE_OBJECT={obj} {exe.rsplit('/', 1)[0]}/melnode-dpctl {args}")

    def udp_bound(m, port=60198):
        return bool(m.succeed(f"ss -Hulnp 'sport = :{port}'").strip())

    def wait_udp_bound(m):
        """The data plane is built a moment after the unit is active."""
        try:
            retry(lambda _: udp_bound(m), timeout_seconds=60)
        except Exception:
            raise Exception("no UDP socket on 60198:\n" + m.succeed(
                "ss -Huanp; ip -o link show; journalctl -u melnode-cp.service -b --no-pager | tail -30"))

    def crash_and_hold(m):
        """Kill melnode-cp the way a crash does and keep systemd from restarting it: its data plane stays."""
        m.succeed("systemctl kill -s KILL --kill-whom=main melnode-cp.service")
        m.succeed("systemctl stop melnode-cp.service")
        m.succeed("test -e /sys/fs/bpf/melnode/dev_map")
        # nothing owns the sockets but the kernel, so the process dying does not close them
        assert udp_bound(m), "the UDP socket went with the process"

    def start_with(m, specialisation):
        m.succeed(f"/run/current-system/specialisation/{specialisation}/bin/switch-to-configuration test")
        m.succeed("systemctl start melnode-cp.service")
        m.wait_for_unit("melnode-cp.service")

    start_all()
    for m in nodes:
        services_up(m)

    with subtest("no device owns the UDP sockets"):
        for m in nodes:
            wait_udp_bound(m)
            assert "hub" not in m.succeed("ip -o link show"), m.succeed("ip -o link show")

    with subtest("links come up and a reaches c through b"):
        b.wait_until_succeeds("test $(curl -sf http://127.0.0.1:60198/links | jq '[.[] | select(.state == \"Up\")] | length') -eq 2", timeout=90)
        a.wait_until_succeeds("ping -c1 -W1 198.18.0.3", timeout=90)
        c.wait_until_succeeds("ping -c1 -W1 198.18.0.1", timeout=90)

    with subtest("the mesh recovers from an unclean restart"):
        a.succeed("systemctl kill -s KILL --kill-whom=main melnode-cp.service")
        a.wait_until_succeeds("systemctl is-active melnode-cp.service")
        a.wait_until_succeeds("ping -c1 -W1 198.18.0.3", timeout=90)
        c.wait_until_succeeds("ping -c1 -W1 198.18.0.1", timeout=90)

    with subtest("a restart after a crash does not keep a link the configuration dropped"):
        def link_ids(m):
            try:
                return sorted(l.split()[0] for l in dpctl(m, "links").splitlines() if l.strip())
            except Exception:  # the new data plane is not up yet
                return None

        assert link_ids(b) == ["1", "3"], link_ids(b)
        crash_and_hold(b)
        start_with(b, "without-c")
        # the new start builds its data plane from its own configuration, which has link 1 only
        retry(lambda _: link_ids(b) == ["1"], timeout_seconds=60)
        a.wait_until_succeeds("ping -c1 -W1 198.18.0.2", timeout=60)
        a.wait_until_fails("ping -c1 -W1 198.18.0.3", timeout=90)

    with subtest("a restart after a crash builds a fresh data plane with the new configuration"):
        ifindex = lambda m: m.succeed("cat /sys/class/net/node-2/ifindex").strip()
        before = ifindex(a)
        crash_and_hold(a)
        start_with(a, "mtu-1400")
        a.wait_until_succeeds("ping -c1 -W1 198.18.0.2", timeout=90)
        assert ifindex(a) != before, "the data plane the crash left behind was reused"
        assert a.succeed("cat /sys/class/net/node-2/mtu").strip() == "1400"
        a.fail("journalctl -u melnode-cp.service -b | grep -q 'another configuration'")

    with subtest("a clean stop leaves nothing behind"):
        # a still has its link to b; c's was dropped from b's configuration above
        a.succeed("systemctl stop melnode-cp.service")
        assert a.succeed("ip -o link show | grep -c 'node-' || true").strip() == "0", a.succeed("ip -o link show")
        assert not udp_bound(a), "the UDP socket outlived a clean stop"
        a.succeed("test ! -e /sys/fs/bpf/melnode/dev_map")
        a.succeed("systemctl start melnode-cp.service")
        a.wait_until_succeeds("ping -c1 -W1 198.18.0.2", timeout=90)

    with subtest("a network namespace takes its sockets with it"):
        a.succeed("ip netns add scratch")
        # ip netns exec gives its command a fresh /sys, so the scratch pins go on a bpffs of their own
        a.succeed("mkdir -p /run/scratch-bpf && mount -t bpf bpf /run/scratch-bpf")
        dpctl(a, "-pin /run/scratch-bpf/dp up -id 9 -port 60999 -key /etc/melinoe/wg.privatekey", prefix="ip netns exec scratch ")
        assert "60999" in a.succeed("ip netns exec scratch ss -Hulnp"), "no socket in the namespace"
        before = int(a.succeed("dmesg | grep -c -E 'WARNING|BUG:|Oops|refcount|leak' || true").strip())
        a.succeed("ip netns del scratch")
        a.sleep(2)
        after = int(a.succeed("dmesg | grep -c -E 'WARNING|BUG:|Oops|refcount|leak' || true").strip())
        assert after == before, a.succeed("dmesg | tail -40")
        a.succeed("umount /run/scratch-bpf")
        a.wait_until_succeeds("ping -c1 -W1 198.18.0.2", timeout=60)
  '';
}
