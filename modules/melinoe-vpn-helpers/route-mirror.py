#!/usr/bin/env python3
from __future__ import annotations

import argparse
import errno
import logging
import select
import time
from socket import AF_INET

from pyroute2 import IPRoute
from pyroute2.netlink.exceptions import NetlinkError
from pyroute2.netlink.rtnl import RTMGRP_IPV4_ROUTE, RTMGRP_LINK

log = logging.getLogger("route-mirror")

RTM_F_CLONED = 0x200
RTNH_F_ONLINK = 0x4
HEADER_FIELDS = ("family", "dst_len", "src_len", "tos", "proto", "scope", "type")
PLAIN_ATTRS = (
    "RTA_DST",
    "RTA_SRC",
    "RTA_GATEWAY",
    "RTA_PRIORITY",
    "RTA_PREFSRC",
    "RTA_OIF",
    "RTA_FLOW",
    "RTA_NH_ID",
)
DEBOUNCE = 0.2
DEBOUNCE_MAX = 2.0


def route_table(msg) -> int:
    return msg.get_attr("RTA_TABLE") or msg["table"]


def route_key(msg) -> tuple:
    return (
        msg.get_attr("RTA_DST"),
        msg["dst_len"],
        msg["tos"],
        msg.get_attr("RTA_PRIORITY") or 0,
    )


def metrics_of(msg) -> tuple:
    metrics = msg.get_attr("RTA_METRICS")
    if not metrics:
        return ()
    return tuple(sorted((name, value) for name, value in metrics["attrs"]))


def hops_of(msg, excluded) -> tuple | None:
    hops = msg.get_attr("RTA_MULTIPATH")
    if not hops:
        return None
    kept = []
    for hop in hops:
        if hop["oif"] in excluded:
            continue
        kept.append(
            (
                hop["oif"],
                hop["hops"],
                hop["flags"] & RTNH_F_ONLINK,
                tuple(sorted((name, value) for name, value in hop["attrs"])),
            )
        )
    return tuple(kept)


def describe(msg, excluded: set[int]) -> tuple | None:
    if msg["flags"] & RTM_F_CLONED:
        return None
    oif = msg.get_attr("RTA_OIF")
    if oif is not None and oif in excluded:
        return None

    header = {field: msg[field] for field in HEADER_FIELDS}
    header["flags"] = msg["flags"] & RTNH_F_ONLINK
    attrs = {name: msg.get_attr(name) for name in PLAIN_ATTRS}
    attrs = {name: value for name, value in attrs.items() if value is not None}

    hops = hops_of(msg, excluded)
    if hops is not None:
        if not hops:
            return None
        if len(hops) == 1:
            hop_oif, _, hop_flags, hop_attrs = hops[0]
            attrs["RTA_OIF"] = hop_oif
            attrs.update(dict(hop_attrs))
            header["flags"] |= hop_flags
            hops = None

    return (
        tuple(sorted(header.items())),
        tuple(sorted(attrs.items())),
        metrics_of(msg),
        hops,
    )


def kwarg_name(nla: str) -> str:
    return nla.split("_", 1)[1].lower()


def build(desc: tuple, table: int) -> dict:
    header, attrs, metrics, hops = desc
    spec = dict(header)
    spec["table"] = table
    spec.update((kwarg_name(name), value) for name, value in attrs)
    if metrics:
        spec["metrics"] = {kwarg_name(name): value for name, value in metrics}
    if hops:
        spec["multipath"] = [
            {
                "oif": oif,
                "hops": weight,
                "flags": flags,
                **{kwarg_name(name): value for name, value in hop_attrs},
            }
            for oif, weight, flags, hop_attrs in hops
        ]
    return spec


def label(desc: tuple) -> str:
    header, attrs, _, hops = desc
    attrs = dict(attrs)
    header = dict(header)
    dst = f"{attrs.get('RTA_DST', '0.0.0.0')}/{header['dst_len']}"
    parts = [dst]
    if "RTA_GATEWAY" in attrs:
        parts.append(f"via {attrs['RTA_GATEWAY']}")
    if "RTA_OIF" in attrs:
        parts.append(f"oif {attrs['RTA_OIF']}")
    if hops:
        parts.append(f"{len(hops)} nexthops")
    if "RTA_PRIORITY" in attrs:
        parts.append(f"metric {attrs['RTA_PRIORITY']}")
    return " ".join(parts)


class Mirror:
    def __init__(self, ipr: IPRoute, src: int, dst: int, prefixes: list[str]):
        self.ipr = ipr
        self.src = src
        self.dst = dst
        self.prefixes = tuple(prefixes)
        self.failing: dict[tuple, str] = {}

    def excluded_links(self) -> set[int]:
        return {
            link["index"]
            for link in self.ipr.get_links()
            if (link.get_attr("IFLA_IFNAME") or "").startswith(self.prefixes)
        }

    def routes(self, table: int, excluded: set[int]) -> dict[tuple, tuple]:
        out = {}
        for msg in self.ipr.get_routes(family=AF_INET, table=table):
            if route_table(msg) != table:
                continue
            desc = describe(msg, excluded)
            if desc is not None:
                out[route_key(msg)] = desc
        return out

    def send(self, action: str, desc: tuple) -> str | None:
        try:
            self.ipr.route(action, **build(desc, self.dst))
        except NetlinkError as exc:
            if action == "del" and exc.code == errno.ESRCH:
                return None
            return str(exc)
        return None

    def reconcile(self) -> None:
        excluded = self.excluded_links()
        wanted = self.routes(self.src, excluded)
        current = self.routes(self.dst, set())

        for key, desc in current.items():
            if key in wanted:
                continue
            err = self.send("del", desc)
            if err:
                log.warning("del %s failed: %s", label(desc), err)
            else:
                log.info("del %s", label(desc))

        failing = {}
        for key, desc in wanted.items():
            if current.get(key) == desc:
                continue
            err = self.send("replace", desc)
            if err:
                failing[key] = err
                if self.failing.get(key) != err:
                    log.warning("replace %s failed: %s", label(desc), err)
            else:
                log.info("replace %s", label(desc))
        for key in self.failing.keys() - failing.keys():
            if key in wanted:
                log.info("recovered %s", label(wanted[key]))
        self.failing = failing


def relevant(events: list, dst: int) -> bool:
    for msg in events:
        event = msg.get("event")
        if event in ("RTM_NEWLINK", "RTM_DELLINK"):
            return True
        if event in ("RTM_NEWROUTE", "RTM_DELROUTE") and route_table(msg) != dst:
            return True
    return False


def wait_for_change(events: IPRoute, dst: int, timeout: float) -> None:
    deadline = time.monotonic() + timeout
    changed = False
    settle_by = None
    while True:
        now = time.monotonic()
        limit = settle_by if changed else deadline
        if now >= limit:
            return
        ready, _, _ = select.select([events], [], [], limit - now)
        if not ready:
            return
        try:
            batch = events.get()
        except (NetlinkError, OSError) as exc:
            log.warning("event socket: %s; resyncing", exc)
            return
        if relevant(batch, dst):
            if not changed:
                changed = True
                settle_by = time.monotonic() + DEBOUNCE_MAX
            settle_by = min(settle_by, time.monotonic() + DEBOUNCE)


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Mirror a routing table into another, skipping routes via interfaces with given name prefixes."
    )
    parser.add_argument("--from-table", type=int, default=254)
    parser.add_argument("--to-table", type=int, required=True)
    parser.add_argument("--exclude-prefix", action="append", default=[])
    parser.add_argument("--resync-interval", type=float, default=30.0)
    args = parser.parse_args()

    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")

    with IPRoute() as ipr, IPRoute() as events:
        events.bind(groups=RTMGRP_LINK | RTMGRP_IPV4_ROUTE)
        mirror = Mirror(ipr, args.from_table, args.to_table, args.exclude_prefix)
        while True:
            mirror.reconcile()
            wait_for_change(events, args.to_table, args.resync_interval)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        pass
