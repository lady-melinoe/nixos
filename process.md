# Process For New BGP-Based Rewrite

`ip route | grep -F 'dev vm-' | cut -d' ' -f1 | sed '/\//!s/$/\/32/'` (or get via querying kernel routes directly) Advertise this over BGP as the Container Routes, as a BGP Community

`198.18.0.${nodeID}/32` advertise this as if it was a container route, in the same bgp community as them.

`198.51.100.${nodeid}/32` Advertise this over BGP as the GRE Loopback Destination for this node.

## Routing
We create a route on table DEFAULT to the wireguard IP of the next hop peer, for the Gre Loopback Destinations we receive
eg, for 0 -> 1 -> 2, we get a route on 0's default table to 1 & 2's gre loopback, via 1's wireguard ip.

We then create a GRE tunnel from our GRE Loopback to all GRE loopbacks we know. so 0 would have two tunnels, node-1 and node-2
GRE tunnels have the address 198.18.0.${nodeID} for their side of the interface

we also create a `table 1000+{peerID}`, and in that table, we have `default dev node-{peerID}`. 
we also create an ip rule, saying `from all fwmark 1000+{peerID} lookup 1000+{peerID}`
eg, for 0 -> 1 -> 2, 0 would have ip rules `from all fwmark 1001 lookup 1001` `from all fwmark 1002 lookup 1002`, 
and those tables would be `1001: default dev node-1` `1002: default dev node-2` 

We create a route on table DEFAULT to any containers we know, via the owner of the container IPs' GRE loopback
eg if 2 advertised the container 198.18.5.4/32, we would add the entry `198.18.5.4/32 dev node-2`
