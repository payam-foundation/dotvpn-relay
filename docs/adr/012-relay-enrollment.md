# ADR-012: Relay enrollment replaces the shared relay key

Status: accepted. Supersedes the `X-Relay-Key` part of ADR-005.

## Context

Mode B authenticated with a static `X-Relay-Key` header: replayable, shared by every relay, and the relay terminated TLS, so it saw control-plane plaintext. That contradicted "the relay holds no secrets".

## Decision

A relay has its own Ed25519 identity. An admin creates a relay record on core and gets a one-time, short-lived enrollment token. `GET /v1/relays/enroll/pending` returns the pending addr/`domain`/`dns_carrier` metadata without consuming the token so installers can skip questions already answered on core. `dotvpn-relay enroll` redeems the token once; it is then dead. The relay stores its identity key and control's public key and nothing else. Clients and nodes authenticate to the relay with short-lived tokens signed by control (audiences `""` for users, `pool` for nodes), which the relay verifies offline.

Control keeps a revocation per relay. `DELETE /v1/admin/relays/{id}` removes it from client lists and blocks re-enrollment. The old header grants nothing.

Relay hostname identity is a single `domain` field (TLS SNI and optional DNS carrier zone). See ADR-025. Fields `sni`, `dns_zone`, and `expected_sni` are dropped; SNI ≠ zone guidance is obsolete.

## Community self-registration

Alongside admin enrollment, control exposes `POST /v1/relays/register`. A relay proves possession of its identity key with signature payload `register|{pk}|{port}|{domain}|{dns}|{ts}` (`dns` is `0` or `1`). Control takes the peer IP as the address, probes the host, and may activate or leave it pending. The relay CLI `register` command stores `relay_id`, trusted keys, and a `control_url` override (plus optional TLS pin). Beats are gated by `beat_mode` (ADR-024), not by the presence of `control_url`: empty override resolves to the production default when a non-disabled mode needs control. Community register defaults to `beat_mode=normal` for pool liveness. Curated `enroll` defaults to no override and `beat_mode=disabled`.

## Consequences

A seized curated relay yields its own key (revoked and replaced) and control's public key. It holds no node address, no user identity, no signing key, and (by default) no control URL. A community relay that stored a control override also yields that URL. See ADR-024 for beat modes and bandwidth caps. Flag-day re-enroll/re-register after ADR-025 (no shim for legacy `sni`/`dns_zone`).
