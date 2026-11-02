# ADR-016: Lowkey install

Status: accepted. Extends ADR-015 item 3. ADR-012 to 015 stay in force.

## Context

The audit (Phase 1 report) found that a relay host and a relay binary name the project in many places: the binary and its strings (202 `dotvpn` and 183 `payam` hits after `-s -w`), build info with the module path, every dependency and the VCS revision, function names in `.gopclntab`, the default config path and env var, the usage and error prefix, the metric name, the enrollment token in argv, and (for Docker) image, labels, volume and service names. The repository is public, so any fixed default name, path, port, page or string becomes a signature that can be searched for at scale. Secrecy lives in keys and addresses, never in code.

This reduces fingerprinting. It does not make a relay undetectable, and it does not override a hosting provider's terms. Operators must read and follow their host's acceptable-use policy and the law where they live.

## Decision

1. **Two install modes, one binary behaviour.** `standard` keeps today's names. `--lowkey` derives every install-time identifier from `--name`. The wire protocol, handshake, key derivation, knock format and probe-resistance behaviour do not change. `handshake.Version` and the knock label stay byte-identical.

2. **Two deploy forms.** Native systemd is the form for `--lowkey`. Docker is for `standard` only: image config, labels and history cannot be made neutral by retagging. `install.sh --lowkey` has no Docker path, and the Docker docs make no lowkey claim. Docker remains the quick way to a standard relay.

3. **No default identifier.** `--name` is required to be operator-chosen, or is a random wordlist word plus a hex suffix, printed once at the end. The installer rejects a name that contains `dotvpn`, `payam`, `relay` or `vpn` (case-insensitive) or characters outside `[a-z0-9-]`. A neutral default would become the new signature, so there is none.

4. **Names derive from the program name.** The binary uses `filepath.Base(os.Args[0])` for flag-set names, usage, the stderr prefix and error hints. The default config path is `/etc/<prog>/config.yaml`. The config env var is `<PROG>_CONFIG` (upper case, non-alphanumerics to `_`); `DOTVPN_RELAY_CONFIG` is still accepted. The metric is `<prog>_parked` (non-alphanumerics to `_`). For the stock binary name these equal today's values, so nothing changes for existing installs. The lifecycle log line `relay started` becomes `started`.

5. **Neutral build artifact.** A second goreleaser build id, `neutral`, built with garble (`-literals -tiny`), `-trimpath`, `-buildvcs=false`, `-s -w`, `CGO_ENABLED=0`. It is published beside the stock artifact in the same `checksums.txt`, with an SBOM, and signed by the existing keyless cosign step. Measured: all `dotvpn`/`payam`/wire-string hits go to 0, there is no build info, the output is reproducible, `garble test` passes, and the binary is about 3x larger (30.7 vs 10.5 MB on amd64) with about 11 ms more exec time. Garble is a pinned CI tool, not a `go.mod` dependency, and it must be built with the same Go as `go.mod`. The stock build also gains `-buildvcs=false`. The installer defaults to the neutral artifact in lowkey mode and to the stock one in standard mode.

6. **Enrollment secrets never touch argv or disk.** `enroll -token -` reads the token from stdin. The installer reads its own `ENROLL_TOKEN` environment variable (or stdin), pipes it to `enroll`, and unsets it. The control URL is used once and not persisted, as in `cmdEnroll` today. `config.Save` writes mode 0640, not 0750, because the file holds the identity key (the code comment already says 0600). The shell history of the install command is the operator's: the docs say to start the command with a space and set `HISTCONTROL=ignorespace`, or to pass secrets by environment. The installer does not touch history.

7. **Decoy is the operator's.** The installer does not install or configure a web server. `--decoy HOST:PORT` is required and must point at a real TLS site the operator already runs (ADR-015 item 3). The installer checks only that the address is well-formed. A non-loopback decoy is allowed but prints a warning, since the byte-for-byte replay then crosses the network. Provisioning a decoy (Caddy, nginx, a generated site, ACME, port 80, certificates) was considered and dropped to keep the installer small and to add no surface; every provisioned decoy would also become a shared signature. The README documents what a good decoy needs (real certificate for a real domain, `Server` header and error pages matching, no HTTP/3 advert since the relay is TCP-only, no admin API) and the lab measures nginx and Caddy behind the relay against the same server direct, so operators have measured guidance.

8. **Installer behaviour.** `deploy/install.sh`, modelled on `dotvpn-core/deploy/install.sh`: root check, arch detect, checksum verification, `--upgrade`, `--rollback`, `--uninstall` (by `--name` alone, no project-named state file), `--no-start`, `--no-firewall`, systemd sandboxing as in core. Differences: no sysctl file (a relay does not need one); no data dir for the relay (it writes only at init/enroll); the DNS carrier stays off unless `--dns-zone`; the admin listener moves to a random loopback high port per install; the firewall opens only 443/tcp (plus 53/udp with a DNS zone), and only if ufw or firewalld is already active. It never touches sshd and never touches provider agents. `--quiet-logs` sets `LogLevelMax=warning`; the unit is not silenced by default. Firewall rules the installer adds are not removed by `--uninstall` (they cannot be attributed to an install without a state file), and the uninstall output says so.

9. **Post-install self-audit.** The installer fails (non-zero) if the forbidden tokens appear in any file the installer created, in `/etc/systemd/system`, in `ps`, `ss -tlnp` or `systemctl cat` output for its units, or, on the neutral binary, `dotvpn` or `payam` in `strings`. `relay` and `vpn` are checked on names and paths only, because garble leaves coincidental substrings (`amtrelayhost` from miekg/dns). It prints the report.

## Not done, by design

No hiding of processes, sockets or files from the OS; no hooks, kernel modules or `LD_PRELOAD`; no tampering with provider agents or audit; no log wiping, timestamp changes or history erasing; no claim to a provider that the host is something else. "Secrets out of argv and history" is in scope; erasing evidence is not.

## Consequences

- Names in the installed footprint are operator-chosen or random, so a repository search finds nothing to match. An inspector with root can still see a listener, a process and a unit.
- A neutral release artifact costs a second binary of about 30 MB per arch, a garble-pinned CI step, and a debuggability loss (`-tiny` strips panic positions).
- Interop is unchanged: the neutral binary and the stock binary are the same source and talk to existing clients, nodes and relays.
- Verified on a real host (2026-10-31, Ubuntu 26.04 aarch64, nginx 1.30.6 as decoy): self-audit, rollback on failed enroll, upgrade/rollback/uninstall, and 0 probe differences between the relay port and nginx direct. Details in `dotvpn-lab/docs/hetzner-test-report.md`.

## Cannot be made neutral

Root on the host; traffic shape (ADR-015 item 5); hosting netflow and payment identity; a censor blocking the address outright; a CT-logged domain; Docker image config, labels and history; static config schema keys (`identity_key`, `trusted_keys`, `decoy`); wire constants in other consumers' stock builds.

## Operator responsibilities

Read and follow the host's acceptable-use policy and local law. Keep the host patched. Lowkey mode reduces fingerprinting; it does not make a relay undetectable and does not override a provider's terms.
