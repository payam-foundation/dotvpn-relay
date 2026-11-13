# ADR-024: Beat mode independent of control URL; app-level bandwidth caps

Status: accepted

## Context

Operators confused `control_url` (where control lives) with “beats on”. Curated
enroll deliberately omitted the URL (ADR-012 seizure posture), so `beat enable`
defaulted to production and could miss a self-hosted control. Community relays
must beat within ~90s (`requires_beat` / `relayStaleAfter` on core) to stay in
pools; longer intervals are useful only as telemetry.

Hosting DCs often cap average Mbps (e.g. throttle to 100 Mbps after a 24h
average exceeds 100 Mbps). Relays need local ingress/egress knobs without
touching decoy passthrough (ADR-014).

## Decision

- **`control_url`**: optional override. Empty resolves at runtime to
  `https://ctrl.dotvpn.org/api`. Not a beat switch.
- **`beat_mode`**: `disabled` (default) | `normal` (~45–75s, pool-live) |
  `conservative` (1h, telemetry-only) | `telemetry` (12h, telemetry-only).
- Legacy configs with `control_url` set and no `beat_mode` migrate to `normal`.
- Community `register` stores the control URL used and sets `beat_mode=normal`.
- Curated `enroll` keeps empty override + `disabled` unless the operator opts in
  (`-beat-mode`, `-store-control`, or legacy `-beat`).
- App-level **ingress** and **egress** Mbps caps apply only on Dot VPN splice
  paths. Optional rolling-average soft-cap engages a lower sustained rate.
- Local TUI is the default bare-TTY entry; `status` remains plain text for scripts.

## Consequences

- Core’s 90s stale window is unchanged; non-`normal` modes will drop
  `requires_beat` relays from pools.
- Installer asks for control domain, beat mode, and optional bandwidth separately.
- ADR-012 seizure posture remains: curated disk without override + disabled beats
  still does not point at control.

## Domain checks (follow-on)

Amended by ADR-025 (singular `domain`):

- Host persists `domain` from enroll/register (and local `dns_listen` when the
  carrier is on). Live DNS/TLS/CDN checks (installer, `doctor`, `status`, TUI)
  use `domain`. They never PATCH control’s catalog.
- Dropped: `sni`, `dns_zone`, and “SNI ≠ zone” fallbacks. Catalog
  `addr`/`domain`/`dns_carrier` remain admin-owned. Blocked/compromised relays:
  disable or revoke and enroll a replacement — no host-side catalog edit API.
