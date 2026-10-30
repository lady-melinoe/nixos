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

    def dpctl(m, args):
        """melnode-dpctl against the data plane the running (or last) melnode-cp configured."""
        show = "systemctl show -p ExecStart --value melnode-cp.service"
        exe = m.succeed(f"{show} | grep -o '/nix/store/[^ ;]*/bin/melnode-cp' | head -1").strip()
        conf = m.succeed(f"{show} | grep -o '/nix/store/[^ ;]*melnode-cp.toml' | head -1").strip()
        obj = m.succeed(f"sed -n 's/^ebpfObject = \"\\(.*\\)\"$/\\1/p' {conf}").strip()
        return m.succeed(f"MELNODE_OBJECT={obj} {exe.rsplit('/', 1)[0]}/melnode-dpctl {args}")

    def crash_and_hold(m):
        """Kill melnode-cp the way a crash does and keep systemd from restarting it: its data plane stays."""
        m.succeed("systemctl kill -s KILL --kill-whom=main melnode-cp.service")
        m.succeed("systemctl stop melnode-cp.service")
        m.succeed("test -e /sys/fs/bpf/melnode/dev_map")
        m.succeed("ip link show node-hub")

    def start_with(m, specialisation):
        m.succeed(f"/run/current-system/specialisation/{specialisation}/bin/switch-to-configuration test")
        m.succeed("systemctl start melnode-cp.service")
        m.wait_for_unit("melnode-cp.service")

    start_all()
    for m in nodes:
        services_up(m)

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
        hub = lambda m: m.succeed("cat /sys/class/net/node-hub/ifindex").strip()
        before = hub(a)
        crash_and_hold(a)
        start_with(a, "mtu-1400")
        a.wait_until_succeeds("ping -c1 -W1 198.18.0.2", timeout=90)
        assert hub(a) != before, "the data plane the crash left behind was reused"
        assert a.succeed("cat /sys/class/net/node-2/mtu").strip() == "1400"
        a.fail("journalctl -u melnode-cp.service -b | grep -q 'another configuration'")
  '';
}
