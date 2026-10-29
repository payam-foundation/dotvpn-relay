# ADR-013: Reverse-first relay

Status: accepted. Supersedes the L4 passthrough design of ADR-005.

## Context

Passthrough forwarding made the relay dial the node. Client configs then carried the node's address, or the relay's config did.

## Decision

The relay only listens. Core nodes dial out to it and keep a pool of parked tunnels (pool knock plus a pool token). A client reaches a node by naming the node ID; the relay splices the client onto a parked tunnel. X25519 forward secrecy on every session; pool tunnels rotate by random time or byte budget; idle padding and jittered keepalive. A DNS-shaped UDP carrier covers UDP-friendly, TCP-hostile paths.

The node's address appears in no client list and in no relay config. Clients pin the node's identity key end to end, so the relay cannot read or alter traffic.

## Risks

The node must reach the relay's public IP. If that direction is filtered too, clients fall back to direct, CDN WebSocket, WebTunnel or Snowflake. No ICMP carrier and no raw-socket desync in v1.
