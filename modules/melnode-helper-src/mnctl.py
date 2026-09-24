#!/usr/bin/env python3
"""mnctl: read-only CLI for melnode's introspection API.

    mnctl <links|routes> [-a] [<ip or hostname>[:port]]

  links   one line per melnode link, akin to `vtysh -c 'show bfd peers brief'`
  routes  one row per prefix (winning owner + best path), akin to `vtysh -c 'show ip bgp'`;
          -a also lists alternate paths

With no target, talks to the local node over its control socket
(/run/melnode/control.sock). With a target, talks to that node's read-only
TCP introspection API (default port 60198), so any node can show any other
node's view. Only GETs are ever sent.
"""

import argparse
import http.client
import ipaddress
import json
import os
import socket
import sys

DEFAULT_SOCKET = "/run/melnode/control.sock"
DEFAULT_PORT = 60198
TIMEOUT = 5.0


# ---- transport --------------------------------------------------------------


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path, timeout):
        super().__init__("localhost", timeout=timeout)
        self._path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self._path)


def split_target(target):
    """'host', 'host:port', '1.2.3.4:port', '[v6]:port', bare v6 -> (host, port)."""
    if target.startswith("["):
        host, _, rest = target[1:].partition("]")
        if rest.startswith(":"):
            return host, int(rest[1:])
        return host, DEFAULT_PORT
    if target.count(":") == 1:
        host, port = target.split(":")
        return host, int(port)
    return target, DEFAULT_PORT  # hostname/IPv4 without port, or bare IPv6


class Client:
    def __init__(self, target, socket_path):
        self.target = target
        self.socket_path = socket_path

    def describe(self):
        if self.target is None:
            return self.socket_path
        host, port = split_target(self.target)
        return f"{host}:{port}"

    def get(self, path):
        if self.target is None:
            conn = UnixHTTPConnection(self.socket_path, TIMEOUT)
        else:
            host, port = split_target(self.target)
            conn = http.client.HTTPConnection(host, port, timeout=TIMEOUT)
        try:
            conn.request("GET", path)
            resp = conn.getresponse()
            body = resp.read()
            if resp.status != 200:
                raise RuntimeError(f"GET {path}: HTTP {resp.status} {resp.reason}")
            return json.loads(body)
        finally:
            conn.close()


# ---- formatting helpers -----------------------------------------------------

USE_COLOR = sys.stdout.isatty() and "NO_COLOR" not in os.environ


def color(text, code):
    return f"\033[{code}m{text}\033[0m" if USE_COLOR else text


def state_color(state, padded):
    return color(padded, {"Up": "32", "Init": "33", "Down": "31", "AdminDown": "90"}.get(state, "0"))


def fmt_duration(sec):
    sec = int(sec)
    d, sec = divmod(sec, 86400)
    h, sec = divmod(sec, 3600)
    m, s = divmod(sec, 60)
    if d:
        return f"{d}d{h:02d}h"
    if h:
        return f"{h}h{m:02d}m"
    if m:
        return f"{m}m{s:02d}s"
    return f"{s}s"


def fmt_bytes(n):
    n = float(n)
    for unit in ("B", "K", "M", "G", "T"):
        if n < 1024 or unit == "T":
            return f"{int(n)}{unit}" if unit == "B" else f"{n:.1f}{unit}"
        n /= 1024


def table(rows, headers, aligns=None):
    """Plain fixed-width table. Cells are (text, colored_text|None)."""
    aligns = aligns or ["<"] * len(headers)
    cells = [[c if isinstance(c, tuple) else (str(c), None) for c in r] for r in rows]
    widths = [len(h) for h in headers]
    for r in cells:
        for i, (txt, _) in enumerate(r):
            widths[i] = max(widths[i], len(txt))
    out = [
        "  ".join(f"{h:{a}{w}}" for h, a, w in zip(headers, aligns, widths)).rstrip()
    ]
    for r in cells:
        parts = []
        for (txt, col), a, w in zip(r, aligns, widths):
            pad = f"{txt:{a}{w}}"
            parts.append(pad.replace(txt, col, 1) if col else pad)
        out.append("  ".join(parts).rstrip())
    return "\n".join(out)


# ---- links ------------------------------------------------------------------


def cmd_links(client):
    summary = client.get("/summary")
    links = client.get("/links")
    up = sum(1 for l in links if l["state"] == "Up")

    print(f"Node {summary['local_id']}  ({client.describe()})")
    print(f"Session count: {len(links)}  Up: {up}")
    rows = []
    for l in links:
        hs = l.get("last_handshake_seconds_ago")
        state = l["state"]
        rows.append(
            [
                l["peer_id"],
                (state, state_color(state, state)),
                fmt_duration(l["state_for_seconds"]),
                l.get("endpoint") or "-",
                l["prepend_count"],
                "never" if hs is None else fmt_duration(hs) + " ago",
                fmt_bytes(l["tx_bytes"]),
                fmt_bytes(l["rx_bytes"]),
                l["diag"],
            ]
        )
    print(
        table(
            rows,
            ["Peer", "State", "For", "Endpoint", "Prepend", "Handshake", "Tx", "Rx", "Diag"],
            ["<", "<", ">", "<", ">", ">", ">", ">", "<"],
        )
    )


# ---- routes -----------------------------------------------------------------


def prefix_key(p):
    try:
        net = ipaddress.ip_network(p, strict=False)
        return (0, int(net.network_address), net.prefixlen)
    except ValueError:
        return (1, 0, 0)


def cmd_routes(client, show_all):
    summary = client.get("/summary")
    local = summary["local_id"]
    routes = client.get("/routes")
    prefixes = client.get("/prefixes")

    # dest node -> its paths (best first, as the daemon sorts them)
    paths_to = {r["dest"]: r["paths"] for r in routes}

    def path_row(net, owner, path, is_best, also=""):
        """One table row. Owner is the node originating this path."""
        return [
            "*>" if is_best else "* ",
            net,
            owner,
            "-" if path is None else path["via"],
            "-" if path is None else len(path["path"]),
            "-" if path is None else " ".join(map(str, path["path"])),
            "",
            also,
        ]

    rows = []
    n_prefixes = 0
    n_paths = 0
    claimed_dests = set()

    for p in sorted(prefixes, key=lambda p: prefix_key(p["prefix"])):
        n_prefixes += 1
        net = p["prefix"]
        owner = p.get("owner")
        claimants = p.get("claimants", [])
        claimed_dests.update(claimants)
        others = [str(c) for c in claimants if c != owner]
        also = " ".join(others)

        if owner is None:
            rows.append(["  ", net, "-", "-", "-", "-", "", also])
            continue

        if p.get("owner_is_local"):
            rows.append(["*>", net, "local", "-", 0, "i", "", also])
            n_paths += 1
            alts = []
        else:
            best = next((x for x in paths_to.get(owner, []) if x["best"]), None)
            rows.append(path_row(net, owner, best, True, also))
            n_paths += 1
            alts = [x for x in paths_to.get(owner, []) if x is not best]

        if show_all:
            # Every other path we know to the winner, then paths to the other
            # claimants (valid, but not what we forward on).
            extra = [(owner, x) for x in alts]
            for c in claimants:
                if c != owner and c != local:
                    extra += [(c, x) for x in paths_to.get(c, [])]
            extra.sort(key=lambda t: (t[0] != owner, len(t[1]["path"]), t[1]["via"]))
            for c, x in extra:
                rows.append(path_row("", c, x, False))
                n_paths += 1
        elif alts:
            rows[-1][6] = f"+{len(alts)}"

    # Reachable nodes that own no advertised prefix at all.
    for dest in sorted(paths_to):
        if dest not in claimed_dests and paths_to[dest]:
            best = next((x for x in paths_to[dest] if x["best"]), paths_to[dest][0])
            rows.append(path_row(f"node {dest}", dest, best, True))
            n_paths += 1

    print(f"melnode routes, node {local}  ({client.describe()})")
    print("Owner / Next Hop / Path are node ids. Path: nearest neighbor first, origin last; i = local.")
    print("*> = forwarded via the prefix's winning owner." + ("" if show_all else "  Use -a for alternate paths."))
    print()
    headers = ["", "Network", "Owner", "Next Hop", "Hops", "Path", "Alts", "Also claimed by"]
    aligns = ["<", "<", "<", "<", ">", "<", ">", "<"]
    if show_all:  # alternates are listed inline instead of counted
        rows = [r[:6] + r[7:] for r in rows]
        headers = headers[:6] + headers[7:]
        aligns = aligns[:6] + aligns[7:]
    print(table(rows, headers, aligns))
    print()
    print(f"{n_prefixes} prefixes, {n_paths} paths shown")


# ---- main -------------------------------------------------------------------


def main():
    ap = argparse.ArgumentParser(
        prog="mnctl",
        description="Read-only view of a melnode's state (local socket, or any node over TCP).",
    )
    ap.add_argument("command", choices=["links", "routes"])
    ap.add_argument(
        "target",
        nargs="?",
        help=f"node to query: ip or hostname, optionally :port (default port {DEFAULT_PORT}); omit for this node",
    )
    ap.add_argument(
        "-a", "--all", action="store_true", help="routes: also list alternate paths, not just the best per prefix"
    )
    ap.add_argument("--socket", default=DEFAULT_SOCKET, help=argparse.SUPPRESS)
    args = ap.parse_intermixed_args()

    client = Client(args.target, args.socket)
    try:
        if args.command == "links":
            cmd_links(client)
        else:
            cmd_routes(client, args.all)
    except (OSError, http.client.HTTPException, RuntimeError, ValueError) as e:
        print(f"mnctl: {client.describe()}: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
