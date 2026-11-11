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
        loader = m.succeed("systemctl show -p Environment --value melnode-cp.service | tr ' ' '\\n' | sed -n 's/^MELNODE_LOADER=//p'").strip()
        return m.succeed(f"{prefix}env MELNODE_OBJECT={obj} MELNODE_LOADER={loader} {exe.rsplit('/', 1)[0]}/melnode-dpctl {args}")

    def udp_bound(m, port=60198):
        return bool(m.succeed(f"ss -Hulnp 'sport = :{port}'").strip())

    def wait_udp_bound(m):
        """The data plane is built a moment after the unit is active."""
        try:
            retry(lambda _: udp_bound(m), timeout_seconds=60)
        except Exception:
            raise Exception("no UDP socket on 60198:\n" + m.succeed(
                "ss -Huanp; ip -o link show; journalctl -u melnode-cp.service -b --no-pager | tail -30"))

    def crash_and_hold(m, peer=None, link=None):
        """Kill melnode-cp the way a crash does and keep systemd from restarting it: its data plane goes with it."""
        m.succeed("systemctl kill -s KILL --kill-whom=main melnode-cp.service")
        m.succeed("systemctl stop melnode-cp.service")
        m.wait_until_fails("ip link show mel0", timeout=10)
        m.succeed("test ! -e /sys/fs/bpf/melnode/maps/wg_dev")
        assert not udp_bound(m), "the UDP socket outlived the process"
        if peer is not None:
            peer.wait_until_succeeds(f"curl -sf http://127.0.0.1:60198/links | jq -e '.[] | select(.peer_id == {link}) | .state != \"Up\"'", timeout=5)

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

    with subtest("one device carries the whole mesh"):
        links_b = b.succeed("ip -o link show")
        assert "mel0" in links_b and "node-" not in links_b, links_b
        # a neighbour entry per reachable node maps the address routes go via to the node's id
        neigh = b.succeed("ip -6 neigh show dev mel0")
        for gw, ll in (("fe80::1", "01"), ("fe80::3", "03")):
            assert f"{gw} lladdr {ll} PERMANENT" in neigh, neigh
        route = b.succeed("ip -4 route get 198.18.0.3")
        assert "via inet6 fe80::3 dev mel0" in route, route
        route = a.succeed("ip -4 route get 198.18.0.3")
        assert "via inet6 fe80::3 dev mel0" in route, route
        assert "default via inet6 fe80::1 dev mel0" in b.succeed("ip -4 route show table 1001"), b.succeed("ip -4 route show table 1001")
        ids = b.succeed("nft list map inet mangle melinoe_peer_ids")
        assert "0x1 : 0x000003e9" in ids and "0x3 : 0x000003eb" in ids, ids

    with subtest("a reply goes back the way the request came in"):
        # c owns an address on a vm-* interface, which melnode-helper advertises
        c.succeed("ip link add vm-test type dummy && ip addr add 198.18.1.77/32 dev vm-test && ip link set vm-test up && ip route replace 198.18.1.77/32 dev vm-test")
        a.wait_until_succeeds("ip -4 route get 198.18.1.77 | grep -q 'via inet6 fe80::3 dev mel0'", timeout=60)
        a.wait_until_succeeds("ping -c1 -W1 198.18.1.77", timeout=60)
        # the request was pinned to the node it came from (a, mark 1000 + 1), by the sender's link address
        pinned = c.succeed("grep 198.18.1.77 /proc/net/nf_conntrack")
        assert "mark=1001" in pinned, pinned

    with subtest("the mesh recovers from an unclean restart"):
        a.succeed("systemctl kill -s KILL --kill-whom=main melnode-cp.service")
        a.wait_until_succeeds("systemctl is-active melnode-cp.service")
        a.wait_until_succeeds("ping -c1 -W1 198.18.0.3", timeout=90)
        c.wait_until_succeeds("ping -c1 -W1 198.18.0.1", timeout=90)

    with subtest("a restart after a crash does not keep a link the configuration dropped"):
        def link_ids(m):
            try:
                return sorted(str(l["peer_id"]) for l in links(m))
            except Exception:  # the new data plane is not up yet
                return None

        assert link_ids(b) == ["1", "3"], link_ids(b)
        crash_and_hold(b, a, 2)
        start_with(b, "without-c")
        # the new start builds its data plane from its own configuration, which has link 1 only
        retry(lambda _: link_ids(b) == ["1"], timeout_seconds=60)
        a.wait_until_succeeds("ping -c1 -W1 198.18.0.2", timeout=60)
        a.wait_until_fails("ping -c1 -W1 198.18.0.3", timeout=90)

    with subtest("a restart after a crash builds a fresh data plane with the new configuration"):
        ifindex = lambda m: m.succeed("cat /sys/class/net/mel0/ifindex").strip()
        before = ifindex(a)
        crash_and_hold(a)
        start_with(a, "mtu-1400")
        a.wait_until_succeeds("ping -c1 -W1 198.18.0.2", timeout=90)
        assert ifindex(a) != before, "the data plane the crash left behind was reused"
        assert a.succeed("cat /sys/class/net/mel0/mtu").strip() == "1400"
        a.fail("journalctl -u melnode-cp.service -b | grep -q 'another configuration'")

    with subtest("a clean stop leaves nothing behind"):
        # a still has its link to b; c's was dropped from b's configuration above
        a.succeed("systemctl stop melnode-cp.service")
        assert a.succeed("ip -o link show | grep -c 'mel0' || true").strip() == "0", a.succeed("ip -o link show")
        assert not udp_bound(a), "the UDP socket outlived a clean stop"
        a.succeed("test ! -e /sys/fs/bpf/melnode/maps/wg_dev")
        a.succeed("systemctl start melnode-cp.service")
        a.wait_until_succeeds("ping -c1 -W1 198.18.0.2", timeout=90)

    with subtest("a network namespace takes its sockets with it"):
        a.succeed("ip netns add scratch")
        # ip netns exec gives its command a fresh /sys, so the scratch pins go on a bpffs of their own
        a.succeed("mkdir -p /run/scratch-bpf && mount -t bpf bpf /run/scratch-bpf")
        dpctl(a, "-pin /run/scratch-bpf/dp up -id 9 -port 60999 -key /etc/melinoe/wg.privatekey", prefix="ip netns exec scratch ")
        a.succeed("ip netns exec scratch ip link set mel0 up")
        a.wait_until_succeeds("ip netns exec scratch ss -Hulnp | grep -q 60999", timeout=20)
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
