# ADR-012: Relay enrollment replaces the shared relay key

Status: accepted. Supersedes the `X-Relay-Key` part of ADR-005.

## Context

Mode B authenticated with a static `X-Relay-Key` header: replayable, shared by every relay, and the relay terminated TLS, so it saw control-plane plaintext. That contradicted "the relay holds no secrets".

## Decision

A relay has its own Ed25519 identity. An admin creates a relay record on core and gets a one-time, short-lived enrollment token. `dotvpn-relay enroll` redeems it once; the token is then dead. The relay stores its identity key and control's public key and nothing else. Clients and nodes authenticate to the relay with short-lived tokens signed by control (audiences `""` for users, `pool` for nodes), which the relay verifies offline.

Control keeps a revocation per relay. `DELETE /v1/admin/relays/{id}` removes it from client lists and blocks re-enrollment. The old header grants nothing.

## Community self-registration

Alongside admin enrollment, control exposes `POST /v1/relays/register`. A relay proves possession of its identity key, control takes the peer IP as the address, probes the host, and may activate or leave it pending. The relay CLI `register` command stores `relay_id`, trusted keys, and `control_url` (plus optional TLS pin) so it can send signed `POST /v1/relays/beat` heartbeats. That is a deliberate tradeoff against the curated seizure posture: operators who want the ADR-012 disk surface run curated `enroll`, or community `register` with `run -no-beat` and clear `control_url` / `control_pin`.

## Consequences

A seized curated relay yields its own key (revoked and replaced) and control's public key. It holds no node address, no user identity, no signing key, and no control URL. A community relay that kept beats enabled also yields the control URL it was registered against.
