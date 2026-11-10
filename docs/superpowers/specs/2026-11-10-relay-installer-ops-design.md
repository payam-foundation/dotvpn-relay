# Relay installer preflight + ops CLI

Approved 2026-11-10. Approach B.

## Goals

1. Interactive installer preflight: packages, resources/swap, ports, DNS carrier vs systemd-resolved, firewall, fail2ban (SSH only).
2. DNS conflict when only stub resolver holds loopback :53 → **ask**: fix resolved / skip carrier / abort.
3. `dvrelay` symlink → `dotvpn-relay`; `status` (+ `doctor`); bare TTY invocation prints status.
4. Loopback metrics only; no remote admin; no client IPs in metrics.

## Non-goals

Remote admin API, fail2ban on 443, silent non-interactive host mutation, full TUI hub (later when mutate actions exist).
