#!/usr/bin/env python3
"""mnctl: read-only CLI for melnode's introspection API.

    mnctl <links|routes> [<ip or hostname>[:port]]

  links   one line per melnode link, akin to `vtysh -c 'show bfd peers brief'`
  routes  the path-vector table, akin to `vtysh -c 'show ip bgp'`

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


def cmd_routes(client):
    summary = client.get("/summary")
    local = summary["local_id"]
    routes = client.get("/routes")
    prefixes = client.get("/prefixes")

    # network -> list of (best, via, path)
    table_by_net = {}

    # Locally-originated prefixes: no neighbor, empty path (BGP shows next hop 0.0.0.0).
    for p in prefixes:
        if p.get("locally_advertised") or p.get("owner_is_local"):
            table_by_net.setdefault(p["prefix"], []).append((True, None, []))

    # Learned paths: every prefix the path's origin claims.
    for r in routes:
        for path in r["paths"]:
            nets = path["prefixes"] or [f"node {r['dest']}"]
            for net in nets:
                table_by_net.setdefault(net, []).append(
                    (path["best"], path["via"], path["path"])
                )

    total_paths = sum(len(v) for v in table_by_net.values())
    print(f"melnode path-vector table, local node {local}  ({client.describe()})")
    print("Status codes: * valid, > best")
    print("Next hop / Path are melnode node ids; path is nearest neighbor first, origin last.")
    print()

    rows = []
    for net in sorted(table_by_net, key=prefix_key):
        first = True
        for best, via, path in sorted(
            table_by_net[net], key=lambda t: (not t[0], len(t[2]), t[1] or 0)
        ):
            rows.append(
                [
                    ("*>" if best else "* "),
                    net if first else "",
                    "local" if via is None else via,
                    len(path),
                    " ".join(map(str, path)) if path else "i",
                ]
            )
            first = False
    print(table(rows, ["", "Network", "Next Hop", "Metric", "Path"], ["<", "<", "<", ">", "<"]))
    print()
    print(f"Displayed {len(table_by_net)} network entries and {total_paths} paths")


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
    ap.add_argument("--socket", default=DEFAULT_SOCKET, help=argparse.SUPPRESS)
    args = ap.parse_args()

    client = Client(args.target, args.socket)
    try:
        {"links": cmd_links, "routes": cmd_routes}[args.command](client)
    except (OSError, http.client.HTTPException, RuntimeError, ValueError) as e:
        print(f"mnctl: {client.describe()}: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
