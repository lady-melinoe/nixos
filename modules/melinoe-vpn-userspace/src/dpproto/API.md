# melnode datapath API

The contract between `melnode-cp` (control plane) and a data plane. It is a
generic netlink family, `melnode`, version 3. Two implementations exist or are
planned:

| | userspace data plane (`melnode-dp`, now) | kernel module (later) |
|---|---|---|
| transport | unix `SOCK_SEQPACKET`, one netlink message per datagram | `AF_NETLINK` / `NETLINK_GENERIC` |
| family id | `198` (`FamilyID`), *resolved by the client via `CTRL_CMD_GETFAMILY`*, which the userspace dp answers itself (controller family 16) | dynamic, assigned at registration, resolved the same way |
| events | sent to the attached session (`GETFAMILY` still lists an `events` group, id 1) | multicast group `events` |
| punts | sent to the attached session | unicast to the attached socket's portid |
| how the cp picks it | `dialDataplane` → `dpproto.Dial` | a new backend implementing `dpproto.Datapath` |

The message framing (`nlmsghdr`, `genlmsghdr`, `nlattr`, host byte order,
4-byte alignment, `NLM_F_*`, `NLMSG_ERROR` with extended ack, `NLMSG_DONE`,
64-bit attribute padding) is the real thing, so a kernel backend reuses the
encoder and decoder in `nl.go`/`wire.go` and only replaces the socket.
`melnode_genl.h` holds the command and attribute numbers; a test fails if it
and the Go constants ever disagree.

The control plane only ever uses the `dpproto.Datapath` interface. That
interface, this document and the header are the specification; the userspace
data plane is just its first implementation.

Every client therefore starts the same way: `CTRL_CMD_GETFAMILY` with the name
`"melnode"` to the controller, which returns the family id, its version (checked
against `MELNODE_GENL_VERSION`) and multicast groups; the id then goes in
`nlmsghdr.type` of every request. The family id is never assumed. A request
addressed to a family id the data plane didn't hand out gets `ENOENT`.

## Conventions

* **Requests** are `NLM_F_REQUEST`. Plain commands (*doit*) also set `NLM_F_ACK`
  and complete with `NLMSG_ERROR` (errno 0 = success), preceded by one reply
  message (same command, attributes below) if the command has a reply.
  Getters (*dump*) set `NLM_F_DUMP`; the reply is one `NLM_F_MULTI` message per
  object, ended by `NLMSG_DONE`. An empty dump is just the `NLMSG_DONE`.
* **Errors** are `NLMSG_ERROR` with `-errno` and the text in the extended-ack
  message attribute (`NLM_F_ACK_TLVS`). Codes: `EINVAL` bad or missing
  attribute, `ENOENT`, `EEXIST`, `EIO`, `EOPNOTSUPP` unknown command or API
  version mismatch, `ENODEV` no device configured yet.
* **Attributes** are one flat namespace (`MELNODE_A_*`). Unknown attributes are
  ignored by the userspace data plane, so adding one is not an API break
  there. The kernel module validates strictly (as genetlink does by
  default): an attribute it has no policy for, or a type above its
  `MELNODE_A_MAX`, fails the request with `EINVAL`. A new request attribute
  therefore needs a module that knows it before a control plane may send it.
  `MELNODE_GENL_VERSION` changes only on an incompatible change. Absent optional attributes mean zero/none.
  Peer, node and destination ids are 0..255 (they live in one byte on the
  wire).
* **Notifications** (`PUNT`, `EVENT`) have `seq == 0`, no reply. Both are lossy:
  when the receiver isn't attached or falls behind they are dropped and
  counted (`MELNODE_STAT_PUNT_DROPPED`, `..._EVENTS_DROPPED`), never queued
  without bound. State that matters is recovered with a dump, exactly like
  `ENOBUFS` on a netlink multicast socket.

## Commands

| cmd | kind | request attributes | reply attributes | notes |
|---|---|---|---|---|
| `HELLO` | doit | `API_VERSION` | `API_VERSION`, `PID`?, `CONFIGURED` | `EOPNOTSUPP` on a version mismatch. `PID` is userspace-only. |
| `ATTACH` | doit | none | none | This socket becomes the control plane: punts go to it, events reach it. A later `ATTACH` from another socket replaces it. Sockets that never attach can still issue requests. |
| `DEVICE_SET` | doit | `LOCAL_ID`, `PRIVATE_KEY`, `LISTEN_PORT`, `MTU`, `FWMARK`? | `PUBLIC_KEY` | Configure-once. Identical settings again are a no-op; different ones fail `EEXIST` (`DEVICE_DEL` first). Binds the UDP port and starts the crypto. |
| `DEVICE_DEL` | doit | none | none | Tear everything down (links, tuns, routes, sockets, keys); back to unconfigured, ready for another `DEVICE_SET`. Idempotent. |
| `STATS_GET` | dump | none | one message per counter: `STAT_ID`, `STAT_VALUE` | Ids in `enum melnode_stat`. A data plane may lack counters or have more; the control plane ignores unknown ids. |
| `LINK_ADD` | doit | `PEER_ID`, `PUBLIC_KEY`, `ENDPOINT`? | none | Idempotent. Same key: re-applies `ENDPOINT` (absent leaves it, since a listen-only link learns its peer's address from traffic). Different key: `EEXIST`. |
| `LINK_DEL` | doit | `PEER_ID` | none | `ENOENT` if unknown. Routes using it as next hop are left alone (the control plane removes them); until then their packets are dropped and counted. |
| `LINK_GET` | dump | none | `PEER_ID`, `PUBLIC_KEY`, `ENDPOINT`?, `LAST_HANDSHAKE`, `TX_BYTES`, `RX_BYTES` | |
| `TUN_CREATE` | doit | `PEER_ID` (destination), `TUN_NAME` | `TUN_NAME`, `TUN_STARTED` | Idempotent. A new tun is created stopped: no traffic until `TUN_START`, so the control plane can address it first. The control plane owns naming. |
| `TUN_START` | doit | `PEER_ID` | none | |
| `TUN_DESTROY` | doit | `PEER_ID` | none | Idempotent. |
| `TUN_GET` | dump | none | `PEER_ID`, `TUN_NAME`, `MTU`, `TUN_STARTED` | |
| `ROUTE_SET` | doit | `ROUTE_DST`, `ROUTE_NEXTHOP` | none | Next hop is a link's peer id. |
| `ROUTE_DEL` | doit | `ROUTE_DST` | none | Idempotent. |
| `ROUTE_GET` | dump | none | `ROUTE_DST`, `ROUTE_NEXTHOP` | |
| `INJECT` | no reply | `PKT_LINK`, `PKT_PROTO`, `PKT_DST`, `PKT_TTL`, `PKT_DATA` | | Send a control packet out one link, in the priority class; the data plane stamps its own id as source. Best-effort, no ACK. |
| `PUNT` | notification | | `PKT_LINK` (ingress), `PKT_PROTO`, `PKT_SRC`, `PKT_DST`, `PKT_TTL`, `PKT_DATA` | Any received packet with proto != 0. `PKT_DATA` is everything after the 4-byte routing header, trailing padding included. |
| `EVENT` | notification | | `EVT_KIND`, `PEER_ID`, `EVT_TIME`, `ENDPOINT`? | Kind 1: a Noise handshake completed on the link; `ENDPOINT` is where the peer is now (so a roamed listen-only link is visible). |
| `X_QUIT` | doit | none | none | **Userspace only.** ACKs, then the process exits. Nothing else may depend on it; a kernel data plane has no process. |

The data plane refuses everything but `HELLO`, `ATTACH`, `DEVICE_SET`,
`DEVICE_DEL`, `STATS_GET` and `X_QUIT` with `ENODEV` until it has a device.

## Division of labour (unchanged)

The data plane never decides anything on its own. Per packet it does: tun →
encrypt → next-hop link; link → decrypt → local tun, or re-encrypt onto the
next hop (decrementing TTL, dropping and counting on expiry); proto != 0 →
punt. Handshakes, rekeys, cookies and endpoint roaming are the crypto
protocol's and stay with it. Everything else — liveness, path-vector routing,
which tuns exist, which routes, addressing, hooks, and starting or replacing
the data plane itself — is the control plane's.

## Notes for the kernel implementation

* **Family registration.** `genl_register_family`, `.name = "melnode"`,
  `.version = MELNODE_GENL_VERSION`, one `genl_ops` per command above
  (`GENL_ADMIN_PERM`; dumps use `.dumpit`), `nla_policy` tables built from the
  type comments in the header, and an `events` multicast group.
* **Attach.** Store the caller's `portid` as the upcall socket (like OVS's
  upcall pid) and subscribe events to the group; `PUNT` is `genlmsg_unicast`
  to it. Detaching (socket closed, netlink notifier) leaves forwarding
  running, which is what the control plane relies on when it restarts.
* **Device.** One device per net namespace is enough; `DEVICE_DEL` is
  `wg`-style teardown and the module can then be configured again in place.
  This is also what makes "replace a misconfigured data plane" the same
  operation for both implementations (`DEVICE_DEL`, then `DEVICE_SET`), with
  no process restart.
* **Tuns.** `TUN_CREATE` registers a netdev named by the caller; `TUN_START`
  brings its transmit path up. Keeping this a genl command (instead of
  `RTM_NEWLINK`) is what keeps the control plane's interface a single family.
* **64-bit attributes** are emitted with `nla_put_64bit(..., MELNODE_A_PAD)`;
  the Go encoder emits the same padding attribute.
* **Byte order** is host order everywhere except the port inside a
  `sockaddr`, which is network order like any sockaddr.
* **Endpoint** is a `struct sockaddr_in` (16 bytes) or `sockaddr_in6` (28
  bytes), i.e. what the kernel already holds, never text.
