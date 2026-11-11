# ADR-022: Relay beat includes active count

Status: accepted

## Context

Community relays already send signed beats with `version` and `parked`.
Operators need a coarse “in use” signal without access logs or client IPs
(ADR-014). Local `/metrics` already exposes parked and active gauges.

## Decision

- Beat signed message is
  `beat|{relay_id}|{version}|{parked}|{active}|{timestamp}`.
- JSON body adds `active` (non-negative int from `Server.Active()`).
- Control stores `active` beside `parked` / `version` / `last_seen`.
- Curated relays remain beat-optional (ADR-012). No client IPs, SNIs,
  domains, or keys in beat fields.

## Consequences

- Relay and control bump the signed format together; lifecycle tests cover
  the new message. Control accepts the legacy 5-field signature for one
  release so community relays can upgrade without a hard cutover; omit
  `active` → stored as 0.
