<picture>
  <source media="(prefers-color-scheme: dark)" srcset=".github/readme-header-dark.svg">
  <img alt="Dot VPN" src=".github/readme-header-light.svg">
</picture>

# dotvpn-relay

[![CI](https://github.com/payam-foundation/dotvpn-relay/actions/workflows/ci.yml/badge.svg)](https://github.com/payam-foundation/dotvpn-relay/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/payam-foundation/dotvpn-relay?include_prereleases)](https://github.com/payam-foundation/dotvpn-relay/releases)
[![Status: beta](https://img.shields.io/badge/status-beta-orange)](#status)
[![License: PolyForm Noncommercial](https://img.shields.io/badge/license-PolyForm%20Noncommercial-blue)](LICENSE)

The in-country stealth hop of [Dot VPN](https://github.com/payam-foundation/dotvpn-wire#related-repositories). A relay looks like an ordinary HTTPS website to anyone who is not a Dot VPN client, and splices clients onto tunnels that a `dotvpn-core` node has parked on it.

A relay has **no client code, no admin code, and no user accounts**. It holds its own identity key and control's public key. **Control URL** and **beat mode** are independent: empty `control_url` resolves to `https://ctrl.dotvpn.org/api` when beats need it; `beat_mode` defaults to `disabled`. Curated enroll keeps the seizure posture (no URL override + disabled). Community `register` stores the control used and sets `beat_mode=normal` (required for pool membership; core drops `requires_beat` relays after ~90s without a beat). Modes: `normal` (~45–75s), `conservative` (1h telemetry), `telemetry` (12h), `disabled`.

## Status

Beta. Tested for code correctness and performance on a real host; resistance to a real national censor is **not** validated (see [Threat model](#threat-model)).

## How it works

```mermaid
flowchart LR
    peer(["Any TCP peer<br/>public :443"])
    knock{"HMAC knock valid<br/>and not replayed?"}
    pool[("Parked tunnels<br/>keyed by node ID")]
    node["dotvpn-core node<br/>dials out"]
    decoy["Operator's web server<br/>--decoy HOST:PORT"]

    peer --> knock
    knock -->|"yes: splice frames"| pool
    node -->|"reverse tunnel"| pool
    knock -->|"no: bytes forwarded as-is"| decoy

    classDef focal fill:#fdf0ea,stroke:#eb6c36,color:#2d3142
    classDef ext fill:#f5f5f5,stroke:#7a8399,stroke-dasharray:4 3,color:#2d3142
    class knock focal
    class peer,decoy ext
```

- **Reverse mode.** The relay only listens. The node dials out and parks tunnels, so the node's address is in no client configuration.
- **Mimic carrier.** A Chrome-like ClientHello carries an HMAC knock. Frames are masked and padded, with X25519 forward secrecy, pool rotation and idle cover traffic.
- **Optional DNS-shaped UDP carrier** for paths that pass UDP but not TCP.
- **Decoy for everything else.** Anything that is not a valid, unreplayed knock is handed to a real TLS server and receives that server's answer. There is no silent close or reset on the probe path, and timing stays uniform.
- **Two ways onto the network.** Admins mint a one-time enroll token (curated). Anyone can `register` as a community relay; control probes the host and may quarantine or promote it. Optional signed beats report version and parked-tunnel count.
- **No access logs.** No persisted client IPs, no version banner. `/healthz` and `/metrics` bind to loopback only. Beats follow `beat_mode` (or `run -beat-mode` / legacy `-no-beat`).

## Quick start

`enroll` and `register` default to the production control plane at `https://ctrl.dotvpn.org/api` (CDN). Pass a bare domain (`-control ctrl.dotvpn.org`) or a full URL; bare domains get `/api` appended. You need either that production control or your own [dotvpn-core](https://github.com/payam-foundation/dotvpn-core).

### Curated (admin token)

```sh
dotvpn-relay init   -config /etc/dotvpn-relay/config.yaml -decoy 127.0.0.1:8443
dotvpn-relay enroll -config /etc/dotvpn-relay/config.yaml -token <one-time token>
dotvpn-relay run    -config /etc/dotvpn-relay/config.yaml
```

The control URL is used for enroll and **not** written to disk by default. Pass `-beat-mode normal` (and `-store-control` for a non-default plane) to opt into beats. For a self-signed control certificate, pass `-control https://…` and `-pin <base64 SHA-256 SPKI>`.

Bare TTY (`dvrelay` / `dotvpn-relay`) opens a local **TUI**. `status` prints plain metrics for scripts (version, parked, active, bytes, sessions, beat mode, bandwidth).

To change beats later: `dotvpn-relay beat normal|conservative|telemetry|disabled`, then restart the unit. `beat control -url …` sets the override. For curated pool membership also set `requires_beat: true` on that relay in admin.

### Tuning (YAML)

| Key | Default | Meaning |
|---|---|---|
| `beat_mode` | `disabled` | `disabled` \| `normal` \| `conservative` \| `telemetry` |
| `control_url` | (empty → production default) | Optional control API override |
| `domain` | from enroll/register | Singular hostname: TLS SNI and DNS carrier zone when enabled; local DNS/TLS checks (catalog updates stay on admin). Obsolete: `sni` / `dns_zone` |
| `max_conns` | 4096 | Concurrent in-flight TCP/DNS sessions |
| `max_pool_per_node` | 16 | Parked reverse tunnels per node |
| `max_pool` | 1024 | Global parked-tunnel cap |
| `bandwidth_ingress_mbps` | 0 | Splice ingress cap (peer→relay); 0 = unlimited |
| `bandwidth_egress_mbps` | 0 | Splice egress cap (relay→peer); 0 = unlimited |
| `bandwidth_avg_window` | unset | e.g. `24h` rolling average soft-cap window |
| `bandwidth_avg_cap_mbps` | unset | Engage throttle when average Mbps exceeds this |
| `bandwidth_throttle_mbps` | unset | Sustained Mbps while soft-cap is engaged |

On Linux, raise `nf_conntrack_max` if you see conntrack drops under load; QUIC idle is governed by wire keepalives (~10s / 2m / 3m).

### Community (open self-registration)

```sh
dotvpn-relay init     -config /etc/dotvpn-relay/config.yaml -decoy 127.0.0.1:8443 -listen PUBLIC_IP:443
dotvpn-relay register -config /etc/dotvpn-relay/config.yaml -domain cdn.example.com -port 443
dotvpn-relay run      -config /etc/dotvpn-relay/config.yaml
```

`register` proves possession of the identity key, stores `relay_id`, control's trusted keys, `control_url` (and `-pin` when given), and `beat_mode=normal`. Share `dvr://<relay_id>` with clients that opt into community relays. Use `beat disabled` or `run -beat-mode disabled` (legacy `-no-beat`) to stop beats (the relay will leave pools after ~90s).

### Docker

Distroless, non-root, read-only root filesystem, capabilities dropped, health check, 1 MB capped log:

```sh
export DOTVPN_VERSION=vX.Y.Z
docker compose -f deploy/docker/compose.yaml up -d
```

Compose comments cover `init` / `enroll` / `register` against the data volume.

### Installer (native systemd)

Prefer download → inspect → run. `deploy/install.sh` installs a release under a hardened systemd unit (dedicated user, 0640 config). SHA-256 checks the binary against `checksums.txt`; when `cosign` is on PATH (or `REQUIRE_COSIGN=1`), the installer also verifies `checksums.txt.sigstore.json`.

**Decoy geometry.** The decoy is a real TLS web server you operate. The relay listens on the public address (typically `PUBLIC_IP:443`); the decoy binds loopback (e.g. `127.0.0.1:8443`). They cannot share the same socket. Point DNS A/AAAA for your domain at the relay host with **DNS-only** records — a proxied CDN terminates TLS and breaks the knock. Interactive install can reuse an existing nginx/Caddy or run `--setup-decoy` to install a system Caddy/nginx for your domain (never embedded in the relay binary).

**Interactive** (TTY; curated asks for the enroll token first and loads listen/domain/DNS from control so those prompts are skipped; community shows this host’s IP then asks domain/listen; decoy always; DNS carrier under Advanced):

```sh
curl -fsSL https://raw.githubusercontent.com/payam-foundation/dotvpn-relay/main/deploy/install.sh -o install.sh
# review install.sh, then:
sudo bash install.sh
```

**Curated (non-interactive):** create the relay in core first (`dvadmin` Relays → Add), then paste the token. `GET /v1/relays/enroll/pending` fills listen/domain/DNS when omitted; decoy is still required:

```sh
curl -fsSL https://raw.githubusercontent.com/payam-foundation/dotvpn-relay/main/deploy/install.sh \
  | sudo ENROLL_TOKEN=<one-time token> bash -s -- \
    --decoy 127.0.0.1:8443
```

**Community (non-interactive):**

```sh
curl -fsSL https://raw.githubusercontent.com/payam-foundation/dotvpn-relay/main/deploy/install.sh \
  | sudo bash -s -- --register --domain relay.example.com \
    --decoy 127.0.0.1:8443 --listen <public-ip>:443
```

Both default `--control` to domain `ctrl.dotvpn.org` (API at `https://ctrl.dotvpn.org/api`). Pass a bare domain; the installer/CLI append `/api`. A curated token must be redeemed against the **same** control that minted it. The enroll token travels by environment or stdin (`enroll -token -`), never argv. Curated non-interactive installs without a token fail unless `--init-only`. Interactive install asks for beat mode and optional bandwidth caps separately from the control domain. Preflight covers packages, disk/mem/swap, listen-port conflicts, firewall/fail2ban asks, and DNS-carrier vs systemd-resolved (`--fix-resolved` / `--skip-dns-carrier`). `--setup-decoy`, `--require-dns`, `--dns-carrier` (UDP `:53`; zone = `--domain`), `--upgrade`, `--rollback`, `--uninstall` (optional `--purge-firewall`), `--pin`, `--beat-mode`, `--no-beat`, `--store-control`, `--bw-ingress` / `--bw-egress` / `--bw-avg-cap` / `--bw-throttle`, `--from-dir` and `--lowkey` are in `install.sh --help`. A failed enroll/register removes what the install created. The DNS carrier (`--dns` / local `dns_listen`) is off by default; it is a UDP/53 protocol suffix, not normal authoritative DNS hosting. **Obsolete:** `--sni`, `--dns-zone`, and any guidance that SNI ≠ zone — one `domain` covers both. Standard installs also place a `dvrelay` symlink to `dotvpn-relay` for local ops (bare `dvrelay` opens the TUI; `dvrelay status` / `dvrelay doctor` for scripts).

### Ports

| Mode | TCP | UDP |
|---|---|---|
| Native systemd | listen port (default 443) | 53 only with `--dns-carrier` / `dns_listen` (zone = `domain`) |
| Docker Compose | `443:443/tcp` | uncomment `53:53/udp` when `dns_listen` is set (`dns_carrier` in catalog) |
| Admin/metrics | loopback only (default `127.0.0.1:9101`) | — |

### Lowkey install

`--lowkey` installs the garble-built `neutral` release asset and derives every name from `--name` (random if omitted, printed once): binary, unit, user, `/etc/<name>`, config env var and metric. Nothing on the host, in `ps`, in `ss`, in the unit text, in journald or in the binary's strings carries the project name, and the installer checks this after install and fails if not. Add `--quiet-logs` to log only warnings. Use the same `--lowkey --name` for `--upgrade`, `--rollback` and `--uninstall`. Docker stays the `standard` form: image names, labels and history cannot be made neutral.

```sh
sudo ENROLL_TOKEN=<token> bash install.sh --lowkey --quiet-logs \
  --decoy 127.0.0.1:8443 --listen <public-ip>:443
```

This lowers fingerprinting by scanners and casual inspection. It does not make a relay undetectable (traffic shape, IP and payment links remain, see the threat model), and it does not override your host's terms: read and follow its acceptable-use policy and the law where you live. Residual risks are listed in the threat model.

### Production guidance

- **Use a real decoy.** Point `-decoy` at a stock nginx or Caddy with a real certificate for a plausible domain (any geography; no software geo-check). It should send no HTTP/3 `alt-svc` (the relay is TCP-only), keep its own `Server` header and error pages, and have no admin API. The built-in decoy (`-tls-cert`, `-tls-key`) exists for tests. Installer `--setup-decoy` can provision a system Caddy/nginx on loopback for your domain.
- **Domain.** One hostname (`domain`): clients and nodes present it as TLS SNI, and it is the DNS carrier zone when the carrier is on. Point DNS-only A/AAAA at this host — no proxied CDN. Obsolete: separate `sni` / `dns_zone` / “SNI ≠ zone”.
- **One relay, many nodes.** Each node parks its own tunnels, keyed by node ID. Give each user cohort its own relay set, so revoking one relay does not affect the others.
- **DNS carrier.** Optional. Enable with installer `--dns-carrier`, CLI `-dns`, or YAML `dns_listen` (zone is always `domain`; catalog `dns_carrier`). If only systemd-resolved's stub holds loopback :53, the installer asks to disable `DNSStubListener`, skip the carrier, or abort (`--fix-resolved` / `--skip-dns-carrier` when non-interactive). A non-stub listener on :53 still fails hard.
- **Community visibility.** Self-registered relays start `public=false`; an admin promotes them with `PATCH /v1/admin/relays/{id}` (`public: true`). They must beat about every 30–45s or they leave node pools after ~90s.

Residual risks: tunnelled traffic keeps its packet-size and timing shape, and hosting netflow, payment identity and host conntrack link a relay to its operator.

## Sizing

The relay splices bytes and holds no user state, so it needs far less than a core node.

Measured on a Hetzner ARM VPS (2 vCPU Neoverse-N1, 3.8 GiB RAM) with the relay and nginx decoy on one box:

| State | Relay CPU | Relay RSS |
| --- | --- | --- |
| idle | under 1% | 14 MB |
| 8 parallel 50 MB downloads, about 12 MB/s (about 100 Mbps) total | about 11% of one core | 22 MiB |

That is about 0.11% of a core per Mbps, so a single core would pass roughly 900 Mbps. This is extrapolated from the 100 Mbps point and was not run to saturation.

| Node | Practical ceiling | Notes |
| --- | --- | --- |
| 1 vCPU, 512 MB | the port speed | RAM is not the limit |
| 2 vCPU, 2 GB | the port speed | what the measurement above ran on |

Pick the relay by its bandwidth allowance and port speed, not CPU or RAM. Set `bandwidth_ingress_mbps`, `bandwidth_egress_mbps` and the average cap to the provider's allowance so the relay throttles itself before the provider does.

## Threat model

| Resists | Does not resist |
|---|---|
| Active probing: non-TLS bytes, wrong or missing SNI, partial hellos, bad or replayed knocks all get the decoy's answer. | Traffic-shape analysis: tunnelled traffic keeps its packet-size and timing profile; padding covers the handshake only. |
| Relay seizure (curated): the host holds only its own identity and control's public key. Revoke it at control and enrol a new one. | Community register stores a `control_url` override; beats follow `beat_mode` (default `normal`). Curated default is no override + `disabled`. |
| Replay: the knock cache and token expiry refuse replays; sessions have forward secrecy. | Hosting-provider netflow, payment identity and host connection tracking, which link a relay to its operator. |
| Fingerprinting: the ClientHello matches current Chrome and replies match stock nginx on the tested host. | A censor that blocks the relay's address outright. Anything not yet captured from a real censored network. |

The [lab](https://github.com/payam-foundation/dotvpn-lab#what-a-green-result-means) checks mechanisms against documented censor behaviour. It does not prove the relay works in any given country.

If a relay is seized or suspected compromised, revoke it at control (`DELETE /v1/admin/relays/{id}`) and replace it with a new identity. Nothing on a curated host reaches core; a community host may still name control in `control_url`.


## Verify a release

```sh
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/payam-foundation/dotvpn-relay/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
sha256sum -c --ignore-missing checksums.txt
```

## Upgrade and rollback

The config format is additive. Identity and enrollment survive a rollback.

| Install | Rollback |
|---|---|
| Installer | `install.sh --upgrade` keeps the previous binary as `<name>.prev`; `install.sh --rollback` restores it. |
| Binary | Keep the previous release as `dotvpn-relay.prev` before replacing, then `mv dotvpn-relay.prev dotvpn-relay && systemctl restart dotvpn-relay`. |
| Docker | Set `DOTVPN_VERSION` to the previous tag and run `docker compose up -d`. |

If control rotated its key, run `enroll` or `register` again. A wire major bump is a flag day for core, relay and client: roll all components together.

## Development

Requires the Go version in `go.mod`.

```sh
go build ./... && go vet ./...
go test -race -count=1 ./...
shellcheck deploy/install.sh deploy/garble-build.sh
```

The installer is tested end to end on a real systemd in `dotvpn-lab` (`suites/deploy`, `TestLowkeyInstall`).

`go.mod` pins an exact `dotvpn-wire` tag. For side-by-side work with a sibling checkout use an untracked `go.work` in the parent directory (see CONTRIBUTING.md). CI tests against the pinned wire tag and wire `main`. The relay never imports core or client; anything shared lives in `dotvpn-wire`.

Read [CONTRIBUTING.md](CONTRIBUTING.md) first. Report vulnerabilities as described in [SECURITY.md](SECURITY.md).

## Related repositories

| Repository | Role |
|---|---|
| [dotvpn-wire](https://github.com/payam-foundation/dotvpn-wire) | Shared wire protocol |
| [dotvpn-core](https://github.com/payam-foundation/dotvpn-core) | Server: node, control plane |
| [dotvpn-client](https://github.com/payam-foundation/dotvpn-client) | Client library and `dotvpn` CLI |
| [dotvpn-android](https://github.com/payam-foundation/dotvpn-android) | Android app |
| [dotvpn-lab](https://github.com/payam-foundation/dotvpn-lab) | Censorship lab and black-box tests |

## License

[PolyForm Noncommercial License 1.0.0](LICENSE). Source-available, not open source: personal, research, educational and non-profit use is permitted; commercial use needs a separate licence from Payam Foundation.
