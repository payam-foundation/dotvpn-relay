# ADR-023: Relay beat includes lifetime traffic counters

Status: accepted

## Context

Operators want coarse bandwidth and session counts on community relays
without access logs or client IPs (ADR-014). Local `/metrics` already
exposes parked and active gauges. ADR-022 added `active` to the signed beat.

## Decision

- Beat signed message is
  `beat|{relay_id}|{version}|{parked}|{active}|{bytes_in}|{bytes_out}|{sessions}|{timestamp}`.
- JSON body adds `bytes_in`, `bytes_out`, `sessions` (non-negative ints;
  lifetime since process start).
- Control stores the counters beside parked / version / last_seen.
- Control still accepts ADR-022 (6-field) and pre-active (5-field) signatures
  for one release; omitted traffic fields store as 0.
- Counters are process-lifetime only (no historical time series).

## Consequences

- Relay and control bump the signed format together.
- Restarting a relay resets counters; admin shows last reported values until
  the next beat.
