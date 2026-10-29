# ADR-014: Probe-resistance policy

Status: accepted

## Context

The old relay closed non-TLS and non-allowlisted SNI silently and forwarded any allowed-SNI hello to the node. A silent close or reset differs from a web server and is a probing fingerprint.

## Decision

An unauthenticated connection always gets what a normal web server would give.

- The first bytes are inspected. A ClientHello carrying a valid, unreplayed HMAC knock (random and session-id fields) is a tunnel. Everything else, including non-TLS, wrong or missing SNI, partial hello, bad or replayed knock, is replayed byte for byte to a decoy: a real TLS server (`-decoy`) or a built-in TLS server with a valid certificate. No dotvpn-specific bytes, banner or version string.
- Knocks are bound to a timestamp window and kept in a seen-cache, so a censor replaying a captured hello gets the decoy.
- A pool token must be signed by control; knowing the pool knock parks nothing.
- Frame headers are masked: no constant byte or counter. Idle traffic is padded and jittered.
- No access logs, no client IPs persisted, no domains or keys in errors or metrics.
- Direct nodes serve the same decoy for unknown paths, and the secret carrier paths look identical to a missing path when unauthenticated.

Verified in `dotvpn-lab` (`suites/functional/replay_probe_test.go`, `middlebox_test.go`): relay and decoy responses are compared per probe type, and median response time must match within 40 ms.

## Not covered

JA3/JA4 and packet-size histograms against a stock server need a capture against a real deployment; see `dotvpn-lab/docs/REPORT.md`.
