#!/usr/bin/env bash
# Relay installer (native systemd).
#
# Non-interactive (safe for curl|bash; pass all required flags):
#   curl -fsSL …/deploy/install.sh | sudo ENROLL_TOKEN=… bash -s -- --decoy 127.0.0.1:8443
#     (token loads listen/domain/dns from control; pass --listen/--sni to override)
#   curl -fsSL …/deploy/install.sh | sudo bash -s -- --register --sni relay.example.com \
#     --decoy 127.0.0.1:8443 --listen PUBLIC_IP:443
#
# Interactive (download first so stdin is a TTY):
#   curl -fsSL …/deploy/install.sh -o install.sh && sudo bash install.sh
#
# Flags: --control DOMAIN   control domain (default ctrl.dotvpn.org). Full
#                           https://…/api URLs are accepted and normalized.
#        --register         community self-registration (no admin token; needs --sni)
#        --sni HOST         domain/SNI clients and nodes present (required with --register;
#                           recommended with curated enroll so control can match)
#        --pin PIN          base64 SHA-256 SPKI pin for a self-signed control
#        --decoy HOST:PORT  your real TLS web server (required unless --setup-decoy)
#        --setup-decoy      guided system Caddy/nginx on loopback for --sni
#        --listen ADDR      public listen address (host:port or :port; default PUBLIC_IP:443)
#        --dns-zone Z       enable the DNS carrier on :53 for zone Z (off by default)
#        --require-dns      fail if --sni does not resolve to this host
#        --init-only        install binary+config without enroll/register/start
#        --lowkey           neutral build; every name derives from --name (see ADR-016)
#        --name N           lowkey install name (random if omitted, printed at the end)
#        --quiet-logs       journald LogLevelMax=warning for the unit
#        --no-beat          disable telemetry beats after community register
#        --version vX.Y.Z   release to install (default latest)
#        --from-dir DIR     use binaries and checksums.txt from DIR instead of downloading
#        --token-stdin      read the enrollment token from stdin (else ENROLL_TOKEN, else a prompt)
#        --non-interactive  never prompt (implied when stdin is not a TTY or CI is set)
#        --purge-firewall   with --uninstall, also remove ports this installer opened
#        --upgrade | --rollback | --uninstall | --no-start | --no-firewall
# Prefer: download the script, inspect it, then run. Checksums authenticate the
# binary only relative to checksums.txt; when cosign is installed (or
# REQUIRE_COSIGN=1), the installer verifies checksums.txt.sigstore.json.
# Upgrade, rollback and uninstall need the same --lowkey/--name as the install.
# Curated enroll never stores --control. Community --register stores it for beats.
# Decoy is the operator's real site (ADR-015/016). --setup-decoy may install a
# system Caddy/nginx for the operator domain; Caddy is never embedded in the binary.
set -euo pipefail

REPO="${RELAY_REPO:-payam-foundation/dotvpn-relay}"
CONTROL_DOMAIN="ctrl.dotvpn.org"
CONTROL_MOUNT="/api"
CONTROL="https://${CONTROL_DOMAIN}${CONTROL_MOUNT}"
PIN="" DECOY="" LISTEN="" DNS_ZONE="" SNI="" REGISTER=0 NO_BEAT=0
LOWKEY=0 NAME="" QUIET=0
VERSION=latest FROM_DIR="" TOKEN_STDIN=0 ACTION=install START=1 FIREWALL=1
NONINTERACTIVE=0 PURGE_FW=0 LISTEN_SET=0 REGISTER_SET=0 CONTROL_SET=0
SETUP_DECOY=0 REQUIRE_DNS=0 INIT_ONLY=0
DECOY_SITE_ROOT=""

log() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }
usage() { sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" 2>/dev/null | sed '$d' | sed 's/^# \{0,1\}//' || true; }

# Accepts a bare domain or https URL; always builds https://DOMAIN/api.
set_control_domain() {
  local raw host
  raw=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | sed 's|^https\?://||; s|/.*||; s|:.*||; s/[[:space:]]//g')
  host=$raw
  printf '%s' "$host" | grep -Eq '^[a-z0-9]([a-z0-9.-]*[a-z0-9])?\.[a-z]{2,}$' \
    || die "control domain looks invalid: $1 (want example.com)"
  CONTROL_DOMAIN=$host
  CONTROL="https://${CONTROL_DOMAIN}${CONTROL_MOUNT}"
}

need() { [ $# -ge 2 ] || die "$1 needs a value"; }
while [ $# -gt 0 ]; do
  case "$1" in
    --control) need "$@"; set_control_domain "$2"; CONTROL_SET=1; shift 2 ;;
    --register) REGISTER=1; REGISTER_SET=1; shift ;;
    --sni) need "$@"; SNI=$2; shift 2 ;;
    --pin) need "$@"; PIN=$2; shift 2 ;;
    --decoy) need "$@"; DECOY=$2; shift 2 ;;
    --setup-decoy) SETUP_DECOY=1; shift ;;
    --listen) need "$@"; LISTEN=$2; LISTEN_SET=1; shift 2 ;;
    --dns-zone) need "$@"; DNS_ZONE=$2; shift 2 ;;
    --require-dns) REQUIRE_DNS=1; shift ;;
    --init-only) INIT_ONLY=1; START=0; shift ;;
    --lowkey) LOWKEY=1; shift ;;
    --name) need "$@"; NAME=$2; shift 2 ;;
    --quiet-logs) QUIET=1; shift ;;
    --no-beat) NO_BEAT=1; shift ;;
    --version) need "$@"; VERSION=$2; shift 2 ;;
    --from-dir) need "$@"; FROM_DIR=$2; shift 2 ;;
    --token-stdin) TOKEN_STDIN=1; shift ;;
    --non-interactive) NONINTERACTIVE=1; shift ;;
    --purge-firewall) PURGE_FW=1; shift ;;
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

# Piped curl|bash has non-TTY stdin — never prompt.
if [ ! -t 0 ] || [ -n "${CI:-}" ]; then
  NONINTERACTIVE=1
fi

detect_public_ip() {
  local ip=""
  if command -v ip >/dev/null; then
    ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1;i<=NF;i++) if ($i=="src") {print $(i+1); exit}}')
  fi
  if [ -z "$ip" ] && command -v hostname >/dev/null; then
    ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  fi
  case "$ip" in
    ""|127.*|10.*|192.168.*|172.1[6-9].*|172.2[0-9].*|172.3[0-1].*|169.254.*) printf '' ;;
    *) printf '%s' "$ip" ;;
  esac
}

detect_any_ipv4() {
  local ip=""
  if command -v ip >/dev/null; then
    ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1;i<=NF;i++) if ($i=="src") {print $(i+1); exit}}')
  fi
  if [ -z "$ip" ] && command -v hostname >/dev/null; then
    ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  fi
  printf '%s' "$ip"
}

prompt() { # var prompt default
  local _var=$1 _p=$2 _d=${3:-} _ans=""
  if [ -r /dev/tty ]; then
    if [ -n "$_d" ]; then printf '%s [%s]: ' "$_p" "$_d" > /dev/tty
    else printf '%s: ' "$_p" > /dev/tty; fi
    IFS= read -r _ans < /dev/tty || true
  fi
  [ -n "$_ans" ] || _ans=$_d
  printf -v "$_var" '%s' "$_ans"
}

menu() { # var prompt options...  (numbered; stores selected value in var)
  local _var=$1 _p=$2; shift 2
  local -a _opts=("$@")
  local _i=1 _ans="" _n=${#_opts[@]}
  printf '%s\n' "$_p" > /dev/tty
  for opt in "${_opts[@]}"; do
    printf '  %d) %s\n' "$_i" "$opt" > /dev/tty
    _i=$((_i + 1))
  done
  printf 'Choice [1]: ' > /dev/tty
  IFS= read -r _ans < /dev/tty || true
  [ -n "$_ans" ] || _ans=1
  case "$_ans" in
    *[!0-9]*|"") die "invalid menu choice" ;;
  esac
  if [ "$_ans" -lt 1 ] || [ "$_ans" -gt "$_n" ]; then
    die "invalid menu choice"
  fi
  printf -v "$_var" '%s' "${_opts[_ans-1]}"
}

# validate_listen accepts host:port, [ipv6]:port, or :port. Rejects bare "2083".
validate_listen() {
  local a=$1
  printf '%s' "$a" | grep -Eq '^(\[[0-9a-fA-F:]+\]|[^:[:space:]]*)?:[0-9]{1,5}$' \
    || die "listen must be host:port or :port (got $a); e.g. $(detect_any_ipv4):443 or :443 — not bare 2083"
  local port=${a##*:}
  if ! [ "$port" -ge 1 ] 2>/dev/null || ! [ "$port" -le 65535 ] 2>/dev/null; then
    die "listen port out of range: $port"
  fi
}

listen_includes_loopback() {
  local a=${1:-:443} host
  case "$a" in
    :*|0.0.0.0:*|"[::]":*|"[::0]":*) return 0 ;;
  esac
  host=${a%:*}
  host=${host#[}; host=${host%]}
  case "$host" in
    ""|127.*|localhost|::1|0.0.0.0|::) return 0 ;;
  esac
  return 1
}

decoy_host_port() {
  DECOY_HOST=${DECOY%:*}
  DECOY_PORT=${DECOY##*:}
  DECOY_HOST=${DECOY_HOST#[}; DECOY_HOST=${DECOY_HOST%]}
}

check_listen_decoy_collision() {
  decoy_host_port
  case "$DECOY_HOST" in 127.*|localhost|::1)
    if [ "$DECOY_PORT" = "$(listen_port)" ] && listen_includes_loopback "${LISTEN:-:443}"; then
      die "listen ${LISTEN:-:443} includes loopback, so decoy $DECOY collides: pass --listen PUBLIC_IP:$(listen_port) or move the web server to another port"
    fi
    ;;
  esac
}

resolve_host_ips() {
  local host=$1
  if command -v getent >/dev/null; then
    getent ahostsv4 "$host" 2>/dev/null | awk '{print $1}' | sort -u
    return 0
  fi
  if command -v dig >/dev/null; then
    dig +short A "$host" 2>/dev/null | sort -u
    return 0
  fi
  printf ''
}

# Cloudflare / common CDN edge ranges used to refuse proxied DNS in front of a relay.
is_cdn_ip() {
  local ip=$1
  case "$ip" in
    104.16.*|104.17.*|104.18.*|104.19.*|104.20.*|104.21.*|104.22.*|104.23.*|104.24.*|104.25.*|104.26.*|104.27.*|104.28.*) return 0 ;;
    172.64.*|172.65.*|172.66.*|172.67.*|173.245.*|188.114.*|190.93.*|197.234.*|198.41.*) return 0 ;;
  esac
  return 1
}

this_host_ips() {
  local out="" ip
  ip=$(detect_public_ip)
  [ -n "$ip" ] && out="$ip"
  ip=$(detect_any_ipv4)
  if [ -n "$ip" ] && [[ " $out " != *" $ip "* ]]; then
    out="${out:+$out }$ip"
  fi
  if command -v hostname >/dev/null; then
    for ip in $(hostname -I 2>/dev/null); do
      [[ " $out " == *" $ip "* ]] && continue
      out="${out:+$out }$ip"
    done
  fi
  printf '%s' "$out"
}

check_domain_dns() {
  local host=$1 ips ip match=0 any=0 pub locals
  [ -n "$host" ] || return 0
  printf '%s' "$host" | grep -Eq '^[A-Za-z0-9.-]+$' || die "domain/SNI looks invalid: $host"
  locals=$(this_host_ips)
  pub=$(detect_public_ip)
  [ -n "$pub" ] || pub=$(detect_any_ipv4)
  ips=$(resolve_host_ips "$host")
  if [ -z "$ips" ]; then
    if [ "$REQUIRE_DNS" -eq 1 ] || [ "$NONINTERACTIVE" -eq 0 ]; then
      die "domain $host does not resolve; create a DNS-only A/AAAA to this host (${pub:-unknown IP}), not a proxied/orange-cloud record"
    fi
    warn "domain $host does not resolve yet; clients need a DNS-only A/AAAA to this host (${pub:-unknown}) (use --require-dns to fail hard)"
    return 0
  fi
  while IFS= read -r ip; do
    [ -n "$ip" ] || continue
    any=1
    if is_cdn_ip "$ip"; then
      die "domain $host → CDN edge $ip (proxied/orange-cloud). Relay knock needs DNS-only (grey-cloud) A/AAAA to THIS host (${pub:-unknown}). Cloudflare: DNS → record → Proxy status = DNS only"
    fi
  done <<< "$ips"
  if [ -n "$locals" ]; then
    while IFS= read -r ip; do
      [ -n "$ip" ] || continue
      for local_ip in $locals; do
        [ "$ip" = "$local_ip" ] && match=1
      done
    done <<< "$ips"
    if [ "$match" -eq 0 ]; then
      if [ "$REQUIRE_DNS" -eq 1 ] || [ "$NONINTERACTIVE" -eq 0 ]; then
        die "domain $host → [$(printf '%s' "$ips" | tr '\n' ' ')] but this host is ${pub:-$locals}. Set a DNS-only A/AAAA to ${pub:-that IP} (not CDN)"
      fi
      warn "domain $host → [$(printf '%s' "$ips" | tr '\n' ' ')], this host is ${pub:-$locals} (continuing; pass --require-dns to fail)"
    else
      log "domain $host resolves to this host (${pub:-$locals})"
    fi
  elif [ "$any" -eq 1 ]; then
    warn "could not detect this host's IP to compare with $host → $ips"
  fi
}

# Loads addr/sni/dns from a curated enroll token without consuming it.
# Fills empty LISTEN/SNI/DNS_ZONE so the installer does not re-ask core answers.
apply_enroll_pending() {
  [ "$REGISTER" -eq 0 ] || return 0
  [ -n "${ENROLL_TOKEN:-}" ] || return 0
  command -v curl >/dev/null || die "curl is required to load pending relay metadata from the enroll token"
  command -v python3 >/dev/null || die "python3 is required to parse pending relay metadata from control"
  local url="${CONTROL%/}/v1/relays/enroll/pending" tmp http json parsed p_addr p_sni p_zone name hint=""
  tmp=$(mktemp)
  http=$(curl -sS -o "$tmp" -w '%{http_code}' --max-time 20 \
    -H "Authorization: Bearer ${ENROLL_TOKEN}" -H "Accept: application/json" "$url" 2>/dev/null || printf '000')
  if [ "$http" != "200" ]; then
    if [ "$http" = "401" ]; then
      hint=" Token invalid, expired, or already used — create a new relay in core and paste the new token."
    elif [ "$http" = "404" ] || [ "$http" = "000" ] || grep -qi '<html' "$tmp" 2>/dev/null; then
      hint=" Wrong control domain (API is https://DOMAIN/api; default ctrl.dotvpn.org)."
    fi
    rm -f "$tmp"
    die "could not load pending relay from $url (HTTP ${http}).${hint}"
  fi
  json=$(cat "$tmp")
  rm -f "$tmp"
  parsed=$(printf '%s' "$json" | python3 -c 'import json,sys
d=json.load(sys.stdin)
for k in ("addr","sni","dns_zone","name"):
    print(d.get(k) or "")')
  {
    IFS= read -r p_addr
    IFS= read -r p_sni
    IFS= read -r p_zone
    IFS= read -r name
  } <<< "$parsed"
  if [ -z "$p_addr" ] || [ -z "$p_sni" ]; then
    die "pending relay response missing addr/sni from $url"
  fi
  if [ "$LISTEN_SET" -eq 0 ] || [ -z "$LISTEN" ]; then
    LISTEN=$p_addr
    LISTEN_SET=1
  fi
  if [ -z "$SNI" ]; then
    SNI=$p_sni
  fi
  if [ -z "$DNS_ZONE" ] && [ -n "$p_zone" ]; then
    DNS_ZONE=$p_zone
  fi
  log "from enroll token: name=${name:--} listen=$LISTEN domain=$SNI dns_zone=${DNS_ZONE:--}"
}

check_control_reachable() {
  local url="${CONTROL%/}/healthz" code body
  body=$(mktemp)
  code=$(curl -sS -o "$body" -w '%{http_code}' --max-time 15 "$url" 2>/dev/null || printf '000')
  if [ "$code" != "200" ] || ! grep -qi '^ok' "$body"; then
    rm -f "$body"
    die "control not reachable at $url (HTTP ${code}). Check DNS/TLS for ${CONTROL_DOMAIN} and that core mount is /api"
  fi
  rm -f "$body"
  log "control ok at $CONTROL"
}

check_dns_carrier() {
  [ -n "$DNS_ZONE" ] || return 0
  printf '%s' "$DNS_ZONE" | grep -Eq '^[A-Za-z0-9.-]+$' || die "dns-zone looks invalid: $DNS_ZONE"
  if ss -uln 2>/dev/null | grep -Eq '(:|\.)53\s'; then
    die "UDP/53 is already in use; free it before enabling the DNS carrier, or skip --dns-zone"
  fi
  if command -v systemctl >/dev/null; then
    if systemctl is-active --quiet systemd-resolved 2>/dev/null; then
      warn "systemd-resolved is active; it often binds UDP/53 — the DNS carrier may fail to start"
    fi
  fi
  log "DNS carrier zone=$DNS_ZONE (UDP/53 protocol suffix — not a normal authoritative DNS host)"
}

scan_web_stack() {
  local hits=""
  if systemctl is-active --quiet caddy 2>/dev/null || command -v caddy >/dev/null; then
    hits="${hits}caddy "
  fi
  if systemctl is-active --quiet nginx 2>/dev/null || command -v nginx >/dev/null; then
    hits="${hits}nginx "
  fi
  if ss -tln 2>/dev/null | grep -Eq '(:|\.)443\s'; then
    hits="${hits}listener:443 "
  fi
  printf '%s' "$hits"
}

validate_decoy_tls() {
  local host port
  decoy_host_port
  host=$DECOY_HOST; port=$DECOY_PORT
  (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null || die "nothing answers on $DECOY: start the web server or use guided decoy setup"
  if command -v openssl >/dev/null; then
    openssl s_client -connect "$host:$port" -servername "${SNI:-localhost}" </dev/null 2>/dev/null | grep -q 'BEGIN CERTIFICATE' \
      || die "decoy $DECOY did not complete a TLS handshake"
  fi
}

pkg_install() {
  local pkg=$1
  if command -v apt-get >/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$pkg"
  elif command -v dnf >/dev/null; then
    dnf install -y "$pkg"
  elif command -v yum >/dev/null; then
    yum install -y "$pkg"
  else
    die "no apt/dnf/yum to install $pkg; install a TLS web server yourself and pass --decoy"
  fi
}

setup_decoy_caddy() {
  local domain=$1 root=$2
  command -v caddy >/dev/null || pkg_install caddy
  command -v caddy >/dev/null || die "caddy install failed"
  install -d -m 0755 "$root"
  if [ ! -f "$root/index.html" ]; then
    cat >"$root/index.html" <<EOF
<!doctype html><html lang="en"><head><meta charset="utf-8"><title>$domain</title></head>
<body><h1>$domain</h1><p>Operator site.</p></body></html>
EOF
  fi
  cat >/etc/caddy/Caddyfile <<EOF
{
  auto_https disable_redirects
  http_port 8080
  https_port 8443
}
$domain:8443 {
  bind 127.0.0.1
  root * $root
  file_server
  header -Alt-Svc
}
EOF
  systemctl enable --now caddy
  sleep 1
  DECOY="127.0.0.1:8443"
  log "guided Caddy decoy on $DECOY for $domain (site root $root)"
}

setup_decoy_nginx() {
  local domain=$1 root=$2
  command -v nginx >/dev/null || pkg_install nginx
  command -v nginx >/dev/null || die "nginx install failed"
  install -d -m 0755 "$root" /etc/nginx/ssl
  if [ ! -f "$root/index.html" ]; then
    cat >"$root/index.html" <<EOF
<!doctype html><html lang="en"><head><meta charset="utf-8"><title>$domain</title></head>
<body><h1>$domain</h1><p>Operator site.</p></body></html>
EOF
  fi
  if [ ! -f /etc/nginx/ssl/decoy.crt ]; then
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
      -keyout /etc/nginx/ssl/decoy.key -out /etc/nginx/ssl/decoy.crt \
      -subj "/CN=$domain" -addext "subjectAltName=DNS:$domain" -days 825 2>/dev/null \
      || openssl req -x509 -newkey rsa:2048 -nodes \
      -keyout /etc/nginx/ssl/decoy.key -out /etc/nginx/ssl/decoy.crt \
      -subj "/CN=$domain" -days 825
  fi
  cat >/etc/nginx/sites-available/dotvpn-decoy <<EOF
server {
  listen 127.0.0.1:8443 ssl;
  server_name $domain;
  ssl_certificate /etc/nginx/ssl/decoy.crt;
  ssl_certificate_key /etc/nginx/ssl/decoy.key;
  root $root;
  index index.html;
  add_header Alt-Svc "" always;
}
EOF
  ln -sfn /etc/nginx/sites-available/dotvpn-decoy /etc/nginx/sites-enabled/dotvpn-decoy 2>/dev/null || true
  nginx -t
  systemctl enable --now nginx
  systemctl reload nginx 2>/dev/null || systemctl restart nginx
  DECOY="127.0.0.1:8443"
  log "guided nginx decoy on $DECOY for $domain (site root $root; self-signed until you add a real cert)"
}

run_guided_decoy() {
  local domain=$1 choice="caddy" root
  [ -n "$domain" ] || die "guided decoy needs a domain (--sni)"
  root="${DECOY_SITE_ROOT:-/var/www/$domain}"
  if [ "$NONINTERACTIVE" -eq 0 ]; then
    prompt root "Site root directory for decoy content" "$root"
    if command -v caddy >/dev/null || command -v apt-get >/dev/null; then
      menu choice "Decoy web server" "caddy (recommended)" "nginx"
      case "$choice" in nginx*) choice=nginx ;; *) choice=caddy ;; esac
    else
      choice=nginx
    fi
  else
    if command -v caddy >/dev/null; then choice=caddy
    elif command -v nginx >/dev/null; then choice=nginx
    else choice=caddy
    fi
  fi
  DECOY_SITE_ROOT=$root
  case "$choice" in
    nginx) setup_decoy_nginx "$domain" "$root" ;;
    *) setup_decoy_caddy "$domain" "$root" ;;
  esac
}

show_host_ip_hint() {
  local ips
  ips=$(this_host_ips)
  if [ -n "$ips" ]; then
    printf '%s\n' "This host IP(s): $ips" > /dev/tty
    printf '%s\n' "Point a DNS-only (grey-cloud) A/AAAA at that IP. Proxied/orange-cloud CDN will fail." > /dev/tty
  else
    printf '%s\n' "Could not detect this host's public IP; you will need it for DNS." > /dev/tty
  fi
}

interactive_fill() {
  [ "$ACTION" = install ] || return 0
  [ "$NONINTERACTIVE" -eq 0 ] || return 0
  local mode_choice="" cand="" decoy_choice="" adv_choice="" listen_choice="" stack="" curated_from_token=0
  echo > /dev/tty
  printf '%s\n' "Dot VPN relay installer" > /dev/tty
  printf '%s\n' "Curated: create the relay in core first, then paste only the one-time token here." > /dev/tty
  printf '%s\n' "Community: self-register (no token); you set domain/listen on this host." > /dev/tty
  echo > /dev/tty
  if [ "$REGISTER_SET" -eq 0 ]; then
    menu mode_choice "Mode" "enroll (curated token)" "register (community)" "abort"
    case "$mode_choice" in
      register*) REGISTER=1 ;;
      abort*) die "aborted" ;;
      *) REGISTER=0 ;;
    esac
  fi
  # Control domain only; API path is always /api on that host.
  if [ "$CONTROL_SET" -eq 0 ]; then
    printf '%s\n' "Control domain (API at https://DOMAIN/api). Default is production." > /dev/tty
    prompt CONTROL_DOMAIN "Control domain" "$CONTROL_DOMAIN"
    set_control_domain "$CONTROL_DOMAIN"
    CONTROL_SET=1
  fi
  check_control_reachable
  if [ "$REGISTER" -eq 0 ] && [ "$INIT_ONLY" -eq 0 ]; then
    if [ -z "${ENROLL_TOKEN:-}" ]; then
      printf '%s\n' "On core: Relays → Add (name, public IP, port, domain). Then paste the token below." > /dev/tty
      IFS= read -rsp "enrollment token: " ENROLL_TOKEN < /dev/tty || true
      echo > /dev/tty
    fi
    [ -n "${ENROLL_TOKEN:-}" ] || die "no enrollment token: create a relay in core TUI/CLI first, or pass ENROLL_TOKEN / --init-only"
    apply_enroll_pending
    curated_from_token=1
  fi
  if [ "$curated_from_token" -eq 0 ]; then
    show_host_ip_hint
    echo > /dev/tty
    if [ -z "$SNI" ]; then
      prompt SNI "Domain / SNI (DNS-only A/AAAA to this host)" ""
      [ -n "$SNI" ] || die "domain/SNI is required"
    fi
    if [ "$LISTEN_SET" -eq 0 ]; then
      cand=$(detect_public_ip)
      [ -n "$cand" ] || cand=$(detect_any_ipv4)
      if [ -n "$cand" ]; then
        menu listen_choice "Public listen address" "${cand}:443" "custom"
        if [[ "$listen_choice" == custom ]]; then
          prompt LISTEN "Listen host:port" "${cand}:443"
        else
          LISTEN=$listen_choice
        fi
      else
        prompt LISTEN "Public listen address (host:port or :port)" ":443"
      fi
      LISTEN_SET=1
    fi
  else
    printf '%s\n' "Using listen/domain/dns already set on core for this token." > /dev/tty
  fi
  validate_listen "${LISTEN:-:443}"
  stack=$(scan_web_stack)
  if [ -z "$DECOY" ] && [ "$SETUP_DECOY" -eq 0 ]; then
    [ -n "$stack" ] && printf 'Detected web stack: %s\n' "$stack" > /dev/tty
    menu decoy_choice "Decoy TLS site (probe resistance)" \
      "use / set up existing HOST:PORT" \
      "I already configured a site (enter HOST:PORT)" \
      "guided setup now (system Caddy/nginx on loopback)"
    case "$decoy_choice" in
      guided*) SETUP_DECOY=1 ;;
      *"already"*) prompt DECOY "Decoy HOST:PORT" "127.0.0.1:8443" ;;
      *) prompt DECOY "Decoy HOST:PORT" "127.0.0.1:8443" ;;
    esac
  fi
  # DNS carrier is advanced; curated tokens already carry dns_zone when set on core.
  if [ -z "$DNS_ZONE" ] && [ "$curated_from_token" -eq 0 ]; then
    menu adv_choice "Advanced options" "none (recommended)" "DNS carrier (UDP/53 protocol tunnel)"
    if [[ "$adv_choice" == DNS* ]]; then
      prompt DNS_ZONE "DNS carrier zone suffix (e.g. carrier.example.com)" ""
      [ -n "$DNS_ZONE" ] || die "dns zone required when enabling the DNS carrier"
    fi
  fi
  echo
  log "summary: mode=$([ "$REGISTER" -eq 1 ] && echo register || echo enroll) decoy=${DECOY:-guided} listen=${LISTEN:-:443} sni=${SNI:--} dns_zone=${DNS_ZONE:--} control=$CONTROL"
  local ok=y
  prompt ok "Proceed?" "y"
  case "$ok" in y|Y|yes|YES) ;; *) die "aborted" ;; esac
}

interactive_fill
# Non-interactive: validate control, then pull listen/domain/dns from the token.
if [ "$ACTION" = install ] && [ "$NONINTERACTIVE" -eq 1 ]; then
  check_control_reachable
  if [ "$REGISTER" -eq 0 ] && [ -n "${ENROLL_TOKEN:-}" ]; then
    apply_enroll_pending
  fi
fi

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

verify_checksums_cosign() { # dir with checksums.txt (+ optional .sigstore.json)
  local dir=$1 base want_cosign=0
  if command -v cosign >/dev/null; then want_cosign=1; fi
  [ "${REQUIRE_COSIGN:-0}" = 1 ] && want_cosign=1
  [ "$want_cosign" -eq 1 ] || return 0
  command -v cosign >/dev/null || die "REQUIRE_COSIGN=1 but cosign is not installed"
  if [ ! -f "$dir/checksums.txt.sigstore.json" ]; then
    if [ -n "$FROM_DIR" ]; then
      die "checksums.txt.sigstore.json missing in --from-dir (required when cosign is available or REQUIRE_COSIGN=1)"
    fi
    if [ "$VERSION" = latest ]; then base="https://github.com/$REPO/releases/latest/download"
    else base="https://github.com/$REPO/releases/download/$VERSION"; fi
    fetch "$base/checksums.txt.sigstore.json" "$dir/checksums.txt.sigstore.json"
  fi
  log "verifying checksums.txt with cosign"
  cosign verify-blob --bundle "$dir/checksums.txt.sigstore.json" \
    --certificate-identity-regexp "https://github.com/$REPO/" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    "$dir/checksums.txt" >/dev/null
}

install_binary() {
  local tmp a name base want got
  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' RETURN
  a=$(arch); name="${ASSET_PREFIX}_linux_$a"
  if [ -n "$FROM_DIR" ]; then
    cp "$FROM_DIR/$name" "$tmp/bin"
    [ -f "$FROM_DIR/checksums.txt" ] && cp "$FROM_DIR/checksums.txt" "$tmp/checksums.txt"
    [ -f "$FROM_DIR/checksums.txt.sigstore.json" ] && cp "$FROM_DIR/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json"
  else
    if [ "$VERSION" = latest ]; then base="https://github.com/$REPO/releases/latest/download"
    else base="https://github.com/$REPO/releases/download/$VERSION"; fi
    log "downloading $name ($VERSION)"
    fetch "$base/$name" "$tmp/bin"
    fetch "$base/checksums.txt" "$tmp/checksums.txt"
  fi
  [ -f "$tmp/checksums.txt" ] || die "checksums.txt missing: refusing to install an unverified binary"
  verify_checksums_cosign "$tmp"
  want=$(awk -v n="$name" '$2==n {print $1}' "$tmp/checksums.txt")
  [ -n "$want" ] || die "no checksum for $name"
  got=$(sha256sum "$tmp/bin" | awk '{print $1}')
  [ "$want" = "$got" ] || die "checksum mismatch for $name"
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
  [ -n "$DECOY" ] || die "--decoy HOST:PORT is required: point it at your real TLS web server (ADR-015), or pass --setup-decoy"
  printf '%s' "$DECOY" | grep -Eq '^([^:[:space:]]+|\[[0-9a-fA-F:]+\]):[0-9]{1,5}$' || die "--decoy must be HOST:PORT"
  decoy_host_port
  case "$DECOY_HOST" in
    127.0.0.1|localhost|::1) ;;
    *) warn "decoy $DECOY_HOST is not loopback: probe traffic is replayed to it over the network" ;;
  esac
  check_listen_decoy_collision
  if [ "$SETUP_DECOY" -eq 1 ] || [ "$NONINTERACTIVE" -eq 0 ]; then
    validate_decoy_tls
  else
    (exec 3<>"/dev/tcp/$DECOY_HOST/$DECOY_PORT") 2>/dev/null \
      || warn "nothing answers on $DECOY yet: start the web server before the relay, or probes will not get a site"
  fi
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
  local desc="dotvpn relay" extra="" run_flags="-config $CONF"
  [ "$LOWKEY" -eq 1 ] && desc="$NAME"
  [ "$QUIET" -eq 1 ] && extra="LogLevelMax=warning"
  [ "$NO_BEAT" -eq 1 ] && run_flags="$run_flags -no-beat"
  cat >"$UNIT_FILE" <<EOF
[Unit]
Description=$desc
After=network-online.target
Wants=network-online.target

[Service]
User=$NAME
Group=$NAME
ExecStart=$BIN run $run_flags
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
    printf '%s\n' "$port/tcp" ${DNS_ZONE:+53/udp} >"$CONF_DIR/firewall.ports"
    log "opened $port/tcp${DNS_ZONE:+ and 53/udp} in ufw (uninstall keeps rules unless --purge-firewall)"
  elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
    firewall-cmd -q --permanent --add-port="$port/tcp" ${DNS_ZONE:+--add-port=53/udp} && firewall-cmd -q --reload
    printf '%s\n' "$port/tcp" ${DNS_ZONE:+53/udp} >"$CONF_DIR/firewall.ports"
    log "opened $port/tcp${DNS_ZONE:+ and 53/udp} in firewalld (uninstall keeps rules unless --purge-firewall)"
  else
    warn "no active ufw/firewalld: ensure $port/tcp${DNS_ZONE:+ and 53/udp} are reachable on the host/provider firewall"
  fi
}

purge_firewall() {
  local ports_file="$CONF_DIR/firewall.ports" p
  [ -f "$ports_file" ] || { warn "no $ports_file: nothing to purge"; return 0; }
  while IFS= read -r p; do
    [ -n "$p" ] || continue
    if command -v ufw >/dev/null; then ufw delete allow "$p" >/dev/null 2>&1 || true; fi
    if command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
      firewall-cmd -q --permanent --remove-port="$p" || true
    fi
  done <"$ports_file"
  if command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
    firewall-cmd -q --reload || true
  fi
  log "purged firewall ports listed in $ports_file"
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

listen_port() {
  local a=${LISTEN:-:443}
  case "$a" in
    *:*) printf '%s' "${a##*:}" ;;
    *) printf '%s' 443 ;;
  esac
}

do_enroll() {
  ENROLLED=0
  if [ "$INIT_ONLY" -eq 1 ]; then
    log "init-only: skipping enroll/register"
    return 0
  fi
  if ! grep -q '^trusted_keys: \[\]' "$CONF"; then log "already enrolled"; ENROLLED=1; return; fi
  if [ "$REGISTER" -eq 1 ]; then
    [ -n "$SNI" ] || die "--register needs --sni HOST"
    local args=(register -config "$CONF" -control "$CONTROL" -sni "$SNI" -port "$(listen_port)")
    [ -n "$PIN" ] && args+=(-pin "$PIN")
    if [ -n "$DNS_ZONE" ]; then
      args+=(-dns-port 53 -dns-zone "$DNS_ZONE")
    fi
    # Community register stores control_url (and optional pin) for beats.
    "$BIN" "${args[@]}"
    fix_conf_owner
    ENROLLED=1
    return
  fi
  # Curated enroll requires a token unless --init-only.
  if [ -z "${ENROLL_TOKEN:-}" ] && [ "$TOKEN_STDIN" -eq 0 ] && [ ! -r /dev/tty ]; then
    die "not enrolled: set ENROLL_TOKEN for curated enroll, pass --register --sni HOST, or --init-only"
  fi
  read_token
  local args=(enroll -config "$CONF" -control "$CONTROL" -token -)
  [ -n "$PIN" ] && args+=(-pin "$PIN")
  [ -n "$LISTEN" ] && args+=(-addr "$LISTEN")
  [ -n "$SNI" ] && args+=(-sni "$SNI")
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
    if [ "$PURGE_FW" -eq 1 ]; then purge_firewall; fi
    remove_install
    if [ "$PURGE_FW" -eq 1 ]; then
      log "removed $NAME and purged recorded firewall ports. Revoke the relay at control (DELETE /v1/admin/relays/{id})."
    else
      log "removed $NAME: unit, binary, config and identity key, user. Revoke the relay at control (DELETE /v1/admin/relays/{id}). Firewall rules remain unless you re-run with --purge-firewall before uninstall (ports file is removed with config)."
    fi
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
[ -z "$SNI" ] || [ "$REGISTER" -eq 1 ] || true
[ "$REGISTER" -eq 0 ] || [ -n "$SNI" ] || die "--register needs --sni HOST"
printf '%s' "$CONTROL" | grep -Eq '^https://' || die "--control must be an https URL"
[ "$NO_BEAT" -eq 0 ] || [ "$REGISTER" -eq 1 ] || warn "--no-beat only affects community installs that store control_url"

if [ -n "$LISTEN" ]; then validate_listen "$LISTEN"; fi
if [ -n "$SNI" ]; then check_domain_dns "$SNI"; fi
check_dns_carrier

if [ "$SETUP_DECOY" -eq 1 ]; then
  [ -n "$SNI" ] || die "--setup-decoy needs --sni DOMAIN"
  run_guided_decoy "$SNI"
fi
check_decoy

# Non-interactive curated without token must fail unless --init-only.
if [ "$ACTION" = install ] && [ "$REGISTER" -eq 0 ] && [ "$INIT_ONLY" -eq 0 ] && [ "$NONINTERACTIVE" -eq 1 ]; then
  if [ -z "${ENROLL_TOKEN:-}" ] && [ "$TOKEN_STDIN" -eq 0 ]; then
    die "curated install needs ENROLL_TOKEN (or --token-stdin), --register --sni HOST, or --init-only"
  fi
fi

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
  if [ "$INIT_ONLY" -eq 1 ]; then
    log "init-only complete: enroll/register then systemctl enable --now $NAME"
  elif [ "$ENROLLED" -ne 1 ]; then
    if [ "$REGISTER" -eq 1 ]; then
      warn "not registered: $BIN register -config $CONF -control URL -sni HOST   then   systemctl enable --now $NAME"
    else
      warn "not enrolled: enroll, then start. e.g. printf '%s\\n' \"\$TOKEN\" | $BIN enroll -config $CONF -control URL -token -   then   systemctl enable --now $NAME"
    fi
  fi
  START=0
fi

ROLLBACK=0
[ "$LOWKEY" -eq 1 ] && audit
if [ "$GENERATED_NAME" -eq 1 ]; then
  echo
  log "install name (shown once, needed for --upgrade/--rollback/--uninstall): $NAME"
fi
if [ -n "$SNI" ]; then
  log "domain/sni=$SNI listen=${LISTEN:-:443} decoy=$DECOY"
fi
log "done. Follow your host's acceptable-use policy; lowkey reduces fingerprinting and does not make a relay undetectable."
