# ADR-025: Relay domain is singular

Status: accepted

## Context

Relay identity previously split the operator-facing hostname across `sni`,
`dns_zone`, and sometimes `expected_sni`. Docs and tooling treated SNI and the
DNS carrier zone as independent strings. That duplicated config, invited
mismatches, and made enroll/register/catalog expansion harder than they needed
to be.

## Decision

- One operator-facing field: **`domain`** — a hostname whose DNS is **DNS-only**
  A/AAAA to the relay IP (no proxied CDN).
- That hostname is both **TLS SNI** on TCP `:443` and the **DNS carrier zone**
  on UDP `:53` when the carrier is enabled.
- Dropped: `sni`, `dns_zone`, `expected_sni`, and any separate zone string.
  Prior guidance that SNI ≠ zone is **obsolete and wrong**.
- DNS carrier is opt-in: relay local `dns_listen` plus catalog
  `dns_carrier` bool. At expand time the DNS endpoint is `IP:53` with
  `params.zone=domain`.
- No backward compatibility. Flag-day re-enroll / re-register.
- Community register signature:
  `register|{pk}|{port}|{domain}|{dns}|{ts}` where `dns` is `0` or `1`.

## Consequences

- Host config, enroll pending metadata, admin create, and catalog expansion all
  speak `domain` (+ `dns_carrier` when relevant).
- Installer uses `--domain` / `--dns-carrier`; CLI uses `-domain` / `-dns`.
  Enabling the carrier sets `dns_listen` (zone is always `domain`), never a
  second hostname flag.
- ADR-012, ADR-013, ADR-016, and ADR-024 domain-check language are amended to
  match. Core relay scoping (ADR-019) records `domain` / `dns_carrier` on the
  relay row.
