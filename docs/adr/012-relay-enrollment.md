# ADR-012: Relay enrollment replaces the shared relay key

Status: accepted. Supersedes the `X-Relay-Key` part of ADR-005.

## Context

Mode B authenticated with a static `X-Relay-Key` header: replayable, shared by every relay, and the relay terminated TLS, so it saw control-plane plaintext. That contradicted "the relay holds no secrets".

## Decision

A relay has its own Ed25519 identity. An admin creates a relay record on core and gets a one-time, short-lived enrollment token. `dotvpn-relay enroll` redeems it once; the token is then dead. The relay stores its identity key and control's public key and nothing else. Clients and nodes authenticate to the relay with short-lived tokens signed by control (audiences `""` for users, `pool` for nodes), which the relay verifies offline.

Control keeps a revocation per relay. `DELETE /v1/admin/relays/{id}` removes it from client lists and blocks re-enrollment. The old header grants nothing.

## Consequences

A seized relay yields its own key (revoked and replaced) and control's public key. It holds no node address, no user identity, no signing key.
