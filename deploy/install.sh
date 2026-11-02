#!/usr/bin/env bash
# Relay installer (native systemd). Usage:
#   curl -fsSL https://raw.githubusercontent.com/payam-foundation/dotvpn-relay/main/deploy/install.sh \
#     | sudo ENROLL_TOKEN=... bash -s -- --control https://ctl.example.com --decoy 127.0.0.1:8443 --lowkey
#
# Flags: --control URL      control API to enroll with (used once, never stored)
#        --pin PIN          base64 SHA-256 SPKI pin for a self-signed control
#        --decoy HOST:PORT  your real TLS web server (required)
#        --listen ADDR      public listen address (default :443)
#        --dns-zone Z       enable the DNS carrier on :53 for zone Z (off by default)
#        --lowkey           neutral build; every name derives from --name (see ADR-016)
#        --name N           lowkey install name (random if omitted, printed at the end)
#        --quiet-logs       journald LogLevelMax=warning for the unit
#        --version vX.Y.Z   release to install (default latest)
#        --from-dir DIR     use binaries and checksums.txt from DIR instead of downloading
#        --token-stdin      read the enrollment token from stdin (else ENROLL_TOKEN, else a prompt)
#        --upgrade | --rollback | --uninstall | --no-start | --no-firewall
# Upgrade, rollback and uninstall need the same --lowkey/--name as the install.
# Lowkey lowers fingerprinting. It does not make a relay undetectable and does
# not override your host's terms: read and follow its acceptable-use policy.
set -euo pipefail

REPO="${RELAY_REPO:-payam-foundation/dotvpn-relay}"
CONTROL="" PIN="" DECOY="" LISTEN="" DNS_ZONE="" LOWKEY=0 NAME="" QUIET=0
VERSION=latest FROM_DIR="" TOKEN_STDIN=0 ACTION=install START=1 FIREWALL=1

log() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }
usage() { sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" 2>/dev/null | sed '$d' | sed 's/^# \{0,1\}//' || true; }

need() { [ $# -ge 2 ] || die "$1 needs a value"; }
while [ $# -gt 0 ]; do
  case "$1" in
    --control) need "$@"; CONTROL=$2; shift 2 ;;
    --pin) need "$@"; PIN=$2; shift 2 ;;
    --decoy) need "$@"; DECOY=$2; shift 2 ;;
    --listen) need "$@"; LISTEN=$2; shift 2 ;;
    --dns-zone) need "$@"; DNS_ZONE=$2; shift 2 ;;
    --lowkey) LOWKEY=1; shift ;;
    --name) need "$@"; NAME=$2; shift 2 ;;
    --quiet-logs) QUIET=1; shift ;;
    --version) need "$@"; VERSION=$2; shift 2 ;;
    --from-dir) need "$@"; FROM_DIR=$2; shift 2 ;;
    --token-stdin) TOKEN_STDIN=1; shift ;;
    --upgrade) ACTION=upgrade; shift ;;
    --rollback) ACTION=rollback; shift ;;
    --uninstall) ACTION=uninstall; shift ;;
    --no-start) START=0; shift ;;
    --no-firewall) FIREWALL=0; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown flag: $1" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "run as root (sudo)"
[ "$(uname -s)" = Linux ] || die "the installer supports Linux only"
command -v systemctl >/dev/null || die "systemd is required (use Docker for a standard install without it)"

# Words for a random lowkey name. None may contain a forbidden token.
WORDS=(amber birch cedar dune ember fjord grove harbor iris juniper kelp lumen maple nectar olive pebble quartz reed slate tundra)
FORBIDDEN='dotvpn|payam|relay|vpn'

rand_u16() { od -An -N2 -tu2 /dev/urandom | tr -d ' \n'; }

if [ "$LOWKEY" -eq 1 ]; then
  if [ -z "$NAME" ]; then
    [ "$ACTION" = install ] || die "--name is required with --$ACTION"
    NAME="${WORDS[$(( $(rand_u16) % ${#WORDS[@]} ))]}-$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')"
    GENERATED_NAME=1
  fi
  printf '%s' "$NAME" | grep -Eq '^[a-z][a-z0-9-]{2,24}$' || die "--name must match [a-z][a-z0-9-]{2,24}"
  if printf '%s' "$NAME" | grep -Eqi "$FORBIDDEN"; then die "--name must not contain dotvpn, payam, relay or vpn"; fi
  ASSET_PREFIX=dotvpn-relay-neutral
else
  [ -z "$NAME" ] || die "--name needs --lowkey (a standard install uses the project names)"
  NAME=dotvpn-relay
  ASSET_PREFIX=dotvpn-relay
fi
GENERATED_NAME="${GENERATED_NAME:-0}"

BIN="/usr/local/bin/$NAME"
CONF_DIR="/etc/$NAME"
CONF="$CONF_DIR/config.yaml"
UNIT_FILE="/etc/systemd/system/$NAME.service"

arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
}

fetch() { # url dest
  if command -v curl >/dev/null; then curl -fsSL --retry 3 -o "$2" "$1"
  elif command -v wget >/dev/null; then wget -q -O "$2" "$1"
  else die "curl or wget required"; fi
}

install_binary() {
  local tmp a name base want got
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' RETURN
  a=$(arch); name="${ASSET_PREFIX}_linux_$a"
  if [ -n "$FROM_DIR" ]; then
    cp "$FROM_DIR/$name" "$tmp/bin"
    [ -f "$FROM_DIR/checksums.txt" ] && cp "$FROM_DIR/checksums.txt" "$tmp/checksums.txt"
  else
    if [ "$VERSION" = latest ]; then base="https://github.com/$REPO/releases/latest/download"
    else base="https://github.com/$REPO/releases/download/$VERSION"; fi
    log "downloading $name ($VERSION)"
    fetch "$base/$name" "$tmp/bin"
    fetch "$base/checksums.txt" "$tmp/checksums.txt"
  fi
  # ponytail: sha256 against checksums.txt only; verify checksums.txt itself with
  # cosign (README "Verify a release") when the download channel is not trusted.
  if [ -f "$tmp/checksums.txt" ]; then
    want=$(awk -v n="$name" '$2==n {print $1}' "$tmp/checksums.txt")
    [ -n "$want" ] || die "no checksum for $name"
    got=$(sha256sum "$tmp/bin" | awk '{print $1}')
    [ "$want" = "$got" ] || die "checksum mismatch for $name"
  else
    die "checksums.txt missing: refusing to install an unverified binary"
  fi
  chmod 0755 "$tmp/bin"
  "$tmp/bin" version >/dev/null || die "binary does not run on this host"
  [ -f "$BIN" ] && cp -f "$BIN" "$BIN.prev"
  install -m 0755 "$tmp/bin" "$BIN.new"
  mv -f "$BIN.new" "$BIN"
}

ensure_user() {
  id "$NAME" >/dev/null 2>&1 && die "user $NAME already exists; choose another --name"
  useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$NAME"
  install -d -o root -g "$NAME" -m 0750 "$CONF_DIR"
}

check_decoy() {
  [ -n "$DECOY" ] || die "--decoy HOST:PORT is required: point it at your real TLS web server (ADR-015)"
  printf '%s' "$DECOY" | grep -Eq '^([^:[:space:]]+|\[[0-9a-fA-F:]+\]):[0-9]{1,5}$' || die "--decoy must be HOST:PORT"
  local host=${DECOY%:*} port=${DECOY##*:}
  host=${host#[}; host=${host%]}
  case "$host" in
    127.0.0.1|localhost|::1) ;;
    *) warn "decoy $host is not loopback: probe traffic is replayed to it over the network" ;;
  esac
  if [ "$port" = 443 ] && [ -z "$LISTEN" ]; then
    case "$host" in 127.*|localhost|::1)
      die "the relay listens on :443, which includes loopback, so a decoy on $host:443 collides: pass --listen PUBLIC_IP:443 or move the web server to another port" ;;
    esac
  fi
  (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null || warn "nothing answers on $DECOY yet: start the web server before the relay, or probes will not get a site"
}

pick_admin_port() {
  local p
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    p=$(( $(rand_u16) % 30000 + 20000 ))
    ss -tln 2>/dev/null | grep -q ":$p " || { echo "$p"; return; }
  done
  die "no free admin port found"
}

write_unit() {
  local desc="dotvpn relay" extra=""
  [ "$LOWKEY" -eq 1 ] && desc="$NAME"
  [ "$QUIET" -eq 1 ] && extra="LogLevelMax=warning"
  cat >"$UNIT_FILE" <<EOF
[Unit]
Description=$desc
After=network-online.target
Wants=network-online.target

[Service]
User=$NAME
Group=$NAME
ExecStart=$BIN run -config $CONF
Restart=always
RestartSec=3
LimitNOFILE=1048576
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=true
RestrictSUIDSGID=true
RemoveIPC=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
SystemCallFilter=@system-service
ReadOnlyPaths=$CONF_DIR
$extra

[Install]
WantedBy=multi-user.target
EOF
  chmod 0644 "$UNIT_FILE"
  systemctl daemon-reload
}

open_ports() {
  [ "$FIREWALL" -eq 1 ] || return 0
  local port=${LISTEN##*:}
  port=${port:-443}
  if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
    ufw allow "$port/tcp" >/dev/null
    [ -n "$DNS_ZONE" ] && ufw allow 53/udp >/dev/null
    log "opened $port/tcp${DNS_ZONE:+ and 53/udp} in ufw (uninstall does not remove firewall rules)"
  elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
    firewall-cmd -q --permanent --add-port="$port/tcp" ${DNS_ZONE:+--add-port=53/udp} && firewall-cmd -q --reload
    log "opened $port/tcp${DNS_ZONE:+ and 53/udp} in firewalld (uninstall does not remove firewall rules)"
  fi
}

do_init() {
  if [ -f "$CONF" ]; then log "keeping existing $CONF"; return; fi
  local args=(init -config "$CONF" -decoy "$DECOY")
  [ -n "$LISTEN" ] && args+=(-listen "$LISTEN")
  [ -n "$DNS_ZONE" ] && args+=(-dns-listen :53 -dns-zone "$DNS_ZONE")
  [ "$LOWKEY" -eq 1 ] && args+=(-admin-listen "127.0.0.1:$(pick_admin_port)")
  "$BIN" "${args[@]}"
  fix_conf_owner
}

# enroll rewrites the config by atomic rename, so ownership must be restored after it.
fix_conf_owner() {
  chown root:"$NAME" "$CONF"
  chmod 0640 "$CONF"
}

read_token() {
  if [ -n "${ENROLL_TOKEN:-}" ]; then return; fi
  if [ "$TOKEN_STDIN" -eq 1 ]; then IFS= read -r ENROLL_TOKEN || true
  elif [ -r /dev/tty ]; then IFS= read -rsp "enrollment token: " ENROLL_TOKEN </dev/tty || true; echo >&2
  fi
  [ -n "${ENROLL_TOKEN:-}" ] || die "no enrollment token: set ENROLL_TOKEN, use --token-stdin, or run from a terminal"
}

do_enroll() {
  ENROLLED=0
  if ! grep -q '^trusted_keys: \[\]' "$CONF"; then log "already enrolled"; ENROLLED=1; return; fi
  [ -n "$CONTROL" ] || return 0
  read_token
  local args=(enroll -config "$CONF" -control "$CONTROL" -token -)
  [ -n "$PIN" ] && args+=(-pin "$PIN")
  # The token goes through stdin, never argv. ponytail: the control URL is in argv
  # for the seconds enroll runs (visible to `ps`); it is not stored anywhere.
  printf '%s\n' "$ENROLL_TOKEN" | "$BIN" "${args[@]}"
  unset ENROLL_TOKEN
  fix_conf_owner
  ENROLLED=1
}

# Self-audit (lowkey): no installer-created name, path, file, binary, process,
# unit text or log line may carry the project name.
audit() {
  local bad=0 pid f
  row() { printf '  %-5s %s\n' "$1" "$2"; [ "$1" = FAIL ] && bad=1; return 0; }
  echo; log "self-audit (forbidden tokens: dotvpn payam relay vpn)"
  for f in "$NAME" "$BIN" "$BIN.prev" "$CONF_DIR" "$CONF" "$UNIT_FILE"; do
    if printf '%s' "$f" | grep -Eqi "$FORBIDDEN"; then row FAIL "name: $f"; else row ok "name: $f"; fi
  done
  # Key material is random base64 and can spell a token by chance: skip those lines.
  if grep -vE '^[[:space:]]*(identity_key:|- )' "$CONF" | grep -Eqi "$FORBIDDEN"; then
    row FAIL "content of $CONF (check --decoy host and --dns-zone)"; else row ok "content: $CONF"; fi
  if grep -Eqi "$FORBIDDEN" "$UNIT_FILE"; then row FAIL "content of $UNIT_FILE"; else row ok "content: $UNIT_FILE"; fi
  for f in "$BIN" "$BIN.prev"; do
    [ -f "$f" ] || continue
    if grep -aEqi 'dotvpn|payam' "$f"; then row FAIL "binary strings: $f"; else row ok "binary strings: $f"; fi
  done
  # Other files under /etc/systemd/system are the host's; report, never fail.
  while IFS= read -r f; do
    [ "$f" = "$UNIT_FILE" ] || warn "pre-existing file mentions a token (not created here): $f"
  done < <(grep -rIliE "$FORBIDDEN" /etc/systemd/system 2>/dev/null || true)
  if [ "$START" -eq 1 ] && [ "${ENROLLED:-0}" -eq 1 ]; then
    pid=$(systemctl show -p MainPID --value "$NAME.service")
    if [ -n "$pid" ] && [ "$pid" != 0 ]; then
      if ps -o args= -p "$pid" | grep -Eqi "$FORBIDDEN"; then row FAIL "ps args"; else row ok "ps args: $(ps -o args= -p "$pid")"; fi
      if ss -tlnpu 2>/dev/null | grep "pid=$pid," | grep -Eqi "$FORBIDDEN"; then row FAIL "ss -tlnpu"; else row ok "ss -tlnpu (process column: <$NAME>)"; fi
    else row FAIL "service has no main pid"; fi
    if systemctl cat "$NAME.service" | grep -Eqi "$FORBIDDEN"; then row FAIL "systemctl cat"; else row ok "systemctl cat"; fi
    if journalctl -u "$NAME.service" --no-pager -o cat -n 100 2>/dev/null | grep -Eqi "$FORBIDDEN"; then row FAIL "journald"; else row ok "journald (last 100 lines)"; fi
  fi
  [ "$bad" -eq 0 ] || die "self-audit failed: the install is in place but carries a project name (see FAIL above); fix and rerun, or --uninstall"
  log "self-audit passed"
}

remove_install() {
  systemctl disable --now "$NAME.service" 2>/dev/null || true
  rm -f "$UNIT_FILE" "$BIN" "$BIN.prev" "$BIN.new"
  rm -rf "$CONF_DIR"
  systemctl daemon-reload
  if id "$NAME" >/dev/null 2>&1; then userdel "$NAME" 2>/dev/null || true; fi
  if getent group "$NAME" >/dev/null 2>&1; then groupdel "$NAME" 2>/dev/null || true; fi
}

case "$ACTION" in
  uninstall)
    [ -f "$UNIT_FILE" ] || [ -d "$CONF_DIR" ] || id "$NAME" >/dev/null 2>&1 || die "no install named $NAME"
    remove_install
    log "removed $NAME: unit, binary, config and identity key, user. Revoke the relay at control (DELETE /v1/admin/relays/{id}). Firewall rules, if added, remain."
    exit 0 ;;
  rollback)
    [ -f "$BIN.prev" ] || die "no previous version at $BIN.prev"
    cp -f "$BIN" "$BIN.rolledback"; mv -f "$BIN.prev" "$BIN"; rm -f "$BIN.rolledback"
    systemctl restart "$NAME.service"
    log "rolled back to $("$BIN" version)"
    exit 0 ;;
  upgrade)
    [ -f "$UNIT_FILE" ] || die "no install named $NAME"
    install_binary
    systemctl restart "$NAME.service"
    log "upgraded to $("$BIN" version); previous binary kept as $BIN.prev"
    [ "$LOWKEY" -eq 1 ] && { START=1; ENROLLED=1; audit; }
    exit 0 ;;
esac

[ -f "$UNIT_FILE" ] && die "an install named $NAME exists: use --upgrade, or --uninstall first"
check_decoy
[ -z "$CONTROL" ] || printf '%s' "$CONTROL" | grep -Eq '^https://' || die "--control must be an https URL"
install_binary
# A failure before the service starts (bad token, unreachable control) removes
# what this run created, so the same command can be retried.
ROLLBACK=1
trap '[ "$ROLLBACK" -eq 0 ] || { warn "install failed: removing what it created"; remove_install; }' EXIT
ensure_user
do_init
do_enroll
write_unit
open_ports

if [ "$START" -eq 1 ] && [ "$ENROLLED" -eq 1 ]; then
  systemctl enable --now "$NAME.service"
  sleep 2
  systemctl is-active --quiet "$NAME.service" || { journalctl -u "$NAME.service" -n 30 --no-pager; die "service failed to start"; }
  log "running"
else
  [ "$ENROLLED" -eq 1 ] || warn "not enrolled: enroll, then start. e.g. printf '%s\\n' \"\$TOKEN\" | $BIN enroll -config $CONF -control URL -token -   then   systemctl enable --now $NAME"
  START=0
fi

ROLLBACK=0
[ "$LOWKEY" -eq 1 ] && audit
if [ "$GENERATED_NAME" -eq 1 ]; then
  echo
  log "install name (shown once, needed for --upgrade/--rollback/--uninstall): $NAME"
fi
log "done. Follow your host's acceptable-use policy; lowkey reduces fingerprinting and does not make a relay undetectable."
