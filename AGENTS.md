# dotvpn-relay

In-country stealth hop. Module `github.com/payam-foundation/dotvpn-relay`, binary `dotvpn-relay`. Depends on `dotvpn-wire` only.

## Commands

```sh
go build ./... && go vet ./...
go test -race -count=1 ./...
```

`go.mod` pins an exact `dotvpn-wire` tag and has no `replace`. For side-by-side work use an untracked `go.work` in the parent directory (see CONTRIBUTING.md). CI tests the pinned wire tag and wire `main`.

## Invariants

- Holds no client or admin code. Do not import anything that belongs to core or client.
- Stores no secrets beyond its own identity key and control's public key; no static shared key (ADR-012, ADR-013).
- No access logs, no client IPs, no version banner, no domains or keys in errors or metrics.
- Unauthenticated, invalid, replayed or probing connections are replayed byte for byte to the decoy, never closed or reset; timing stays uniform (ADR-014).
- The built-in decoy is for tests. Production runs `-decoy` in front of a real web server (ADR-015).

## Shared rules

- Static builds: `CGO_ENABLED=0`, `-trimpath`. Docker base images pinned by digest, distroless, non-root.
- Secrets never go in the repo, unit files, env files or CI. Keys are written 0600/0640.
- Go style: stdlib first, no new dependency for what a few lines do, no placeholder code. Mark deliberate shortcuts with a `ponytail:` comment naming the ceiling and the upgrade path.
- Each repo is its own git repo and Go module. Sibling repos live next to this one; do not import across core, relay and client. Anything two of them must agree on goes in `dotvpn-wire`.

## Docs

`docs/adr/`: ADR-012, 013, 014, 015. Security report: `../dotvpn-lab/docs/REPORT.md`.

## Flow

Pull requests only, never push to `main`. PR title is a Conventional Commit, commits are signed off (`git commit -s`), merges are squash-only. Releases come from release-please. See `CONTRIBUTING.md`.
