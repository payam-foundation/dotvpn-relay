# Relay installer + ops CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Smarter relay install preflight and a local `dotvpn-relay status`/`doctor` (symlink `dvrelay`).

**Architecture:** Keep mutations in `deploy/install.sh`; ops read loopback admin + host/proc in `cmd/dotvpn-relay`. DNS stub conflicts are interactive menus; non-interactive needs explicit flags.

**Tech Stack:** bash installer, Go CLI, existing loopback `/healthz` `/metrics`.

## File map

- `deploy/install.sh` — preflight, DNS ask, fw/f2b, swap, `dvrelay` link
- `cmd/dotvpn-relay/main.go` — register status/doctor; default TTY → status
- `cmd/dotvpn-relay/status.go` (+ tests) — status/doctor implementation
- `internal/relay/*.go` — optional byte/conn counters for metrics
- `README.md` — document flags and `dvrelay`

## Tasks

- [x] Fix UDP/53 detection + interactive DNS ask; non-interactive flags
- [x] Preflight: packages, disk/mem/swap ask, TCP listen conflict, fw/f2b asks
- [x] `status`/`doctor` + symlink; extend metrics safely
- [ ] Tests, PR, release, smoke on relay host
