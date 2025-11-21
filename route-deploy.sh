#!/usr/bin/env bash

BASE_PREFIX="198.51.100."
INNER_PREFIX="198.18.0."
TUN_PREFIX="node-"

normalize_prefix() {
    case "$1" in
        */*) echo "$1" ;;
        *)   echo "$1/32" ;;
    esac
}

BGP_JSON=$(vtysh -c 'show bgp ipv4 json')

LOCAL_NODE_ID=$(
  jq -r '
    .routes
    | to_entries[]
    | select(.key | test("^198\\.51\\.100\\.[0-9]+/32$"))
    | .value[0] as $p
    | select($p.path == "" and $p.nexthops[0].ip == "0.0.0.0")
    | .key
  ' <<<"$BGP_JSON" |
  awk -F'[./]' '{print $4}'
)

[ -n "$LOCAL_NODE_ID" ] || exit 1

LOCAL_VIP="${BASE_PREFIX}${LOCAL_NODE_ID}"
LOCAL_INNER="${INNER_PREFIX}${LOCAL_NODE_ID}"
LOCAL_INNER_ROUTE="${LOCAL_INNER}/32"

REMOTE_NODE_IDS=$(
  jq -r '
    .routes
    | to_entries[]
    | select(.key | test("^198\\.51\\.100\\.[0-9]+/32$"))
    | .value[0] as $p
    | select($p.path != "")
    | .key
  ' <<<"$BGP_JSON" |
  awk -F'[./]' '{print $4}' |
  sort -n | uniq
)

EXISTING_TUNNELS=$(
  ip -o link show type gre 2>/dev/null |
  awk -F': ' '{print $2}' |
  cut -d@ -f1 |
  sed 's/:$//' |
  grep "^${TUN_PREFIX}" || true
)

for TUN in $EXISTING_TUNNELS; do
  RID="${TUN#${TUN_PREFIX}}"
  TABLE=$((1000 + RID))
  if ! grep -qx "$RID" <<<"$REMOTE_NODE_IDS"; then
    ip route flush table "$TABLE" >/dev/null 2>&1 || true
    ip rule del fwmark "$TABLE" lookup "$TABLE" >/dev/null 2>&1 || true
    ip link set "$TUN" down >/dev/null 2>&1 || true
    ip tunnel del "$TUN" >/dev/null 2>&1 || true
  fi
done

for RID in $REMOTE_NODE_IDS; do
  R_VIP="${BASE_PREFIX}${RID}"
  R_INNER="${INNER_PREFIX}${RID}"
  TUN="${TUN_PREFIX}${RID}"
  TABLE=$((1000 + RID))

  if ! ip link show "$TUN" >/dev/null 2>&1; then
    ip tunnel add "$TUN" mode gre \
        local "$LOCAL_VIP" \
        remote "$R_VIP" \
        ttl 64
    ip addr add "${LOCAL_INNER}/32" peer "${R_INNER}/32" dev "$TUN"
  fi

  ip link set "$TUN" up || true
  ip rule del fwmark "$TABLE" lookup "$TABLE" >/dev/null 2>&1 || true
  ip rule add fwmark "$TABLE" lookup "$TABLE" >/dev/null 2>&1 || true
  ip route replace default dev "$TUN" table "$TABLE"
done

for RID in $REMOTE_NODE_IDS; do
  R_INNER="${INNER_PREFIX}${RID}"
  TUN="${TUN_PREFIX}${RID}"

  ROUTE_LIST=$(curl -m 5 -sf "http://${R_INNER}:60198/list" || echo "")

  NEW_ROUTES=$(
    echo "$ROUTE_LIST" |
    grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+(/[0-9]+)?$' |
    while read -r p; do normalize_prefix "$p"; done |
    sort -u
  )

  # Currently installed routes for this tunnel
  CURRENT_ROUTES=$(
    ip route show dev "$TUN" |
    awk '{print $1}' |
    while read -r p; do normalize_prefix "$p"; done |
    sort -u
  )

  GRE_PEER_ROUTE="${R_INNER}/32"

  for PREFIX in $NEW_ROUTES; do
    case "$PREFIX" in
      "$LOCAL_INNER_ROUTE") continue ;;
      "$GRE_PEER_ROUTE")    continue ;;
    esac

    if ! grep -qx "$PREFIX" <<<"$CURRENT_ROUTES"; then
      ip route replace "$PREFIX" dev "$TUN"
    fi
  done

  for PREFIX in $CURRENT_ROUTES; do
    case "$PREFIX" in
      "$GRE_PEER_ROUTE") continue ;;
      "$LOCAL_INNER_ROUTE") continue ;;
    esac

    if ! grep -qx "$PREFIX" <<<"$NEW_ROUTES"; then
      ip route del "$PREFIX" dev "$TUN"
    fi
  done
done
