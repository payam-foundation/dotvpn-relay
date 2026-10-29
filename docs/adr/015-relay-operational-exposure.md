# ADR-015: Relay operational exposure

Status: accepted

## Context

The Hetzner audit (`dotvpn-lab/docs/hetzner-test-report.md`, "Relay exposure audit") found that the relay stores nothing sensitive on disk, but an in-country relay host can still be probed or observed. A node has one public IP and cannot dial relays from another, so hiding core's address from the relay host is not available. ADR-012, 013 and 014 stay in force.

## Decision

1. Many nodes per relay, many relays per node. A relay already parks tunnels per node ID and accepts a knock per node with a parked tunnel (`TestRelayServesManyNodes`). Operators run one relay for several nodes, so a relay host's socket table shows a mix of peers, and losing one node or relay does not remove the other.
2. One relay set per user cohort. The client knock key derives from the relay public key and node ID, so an invite proves a relay's role to whoever holds it. A seized or probed relay is revoked at control (`DELETE /v1/admin/relays/{id}`) without touching other cohorts.
3. Real decoy in production. The built-in decoy (Go `net/http`, one static page, test certificate) is for tests. Production relays run `-decoy 127.0.0.1:443` in front of a stock nginx with a real certificate. Measured against stock nginx 1.28 (see the report): ServerHello, non-TLS replies and timing are identical through the relay.
4. The knock ClientHello and every `DialTLS` hello come from one function, `mimic.ChromeCurrent`, which brings the utls Chrome 133 preset to what Chrome 156 sends. Re-measure with `dotvpn-lab/cmd/fpcheck` when browsers move on.
5. Accepted residual risk: tunnelled traffic keeps its packet-size and timing shape (padding covers the handshake only); a relay's idle cover records are the only periodic signal; hosting provider netflow, payment identity and host conntrack link a relay to its operator.
