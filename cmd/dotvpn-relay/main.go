// Package main is the in-country stealth hop. It has no client or admin code.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/payam-foundation/dotvpn-relay/internal/config"
	"github.com/payam-foundation/dotvpn-relay/internal/relay"
	"github.com/payam-foundation/dotvpn-wire/auth"
	"github.com/payam-foundation/dotvpn-wire/loopback"
	"github.com/payam-foundation/dotvpn-wire/transport"
)

var version = "dev"

// defaultControlURL is the production control plane API (CDN fronted).
// Operators override with -control DOMAIN or a full https URL.
const defaultControlURL = "https://ctrl.dotvpn.org/api"

// usage takes the program name and the version. Names come from the binary
// name (see prog), so a renamed binary never prints the project name.
const usage = `%[1]s %[2]s

  init     write a config with a fresh identity key
  enroll   register this relay with control (one-time enrollment token)
  register open self-registration with control (community relays)
  profile  read or update public listing, email and country
  donate   alias: make this community relay public (-public)
  undonate alias: make this community relay private (-private)
  run      serve
  status   local service status (plain text; scripts)
  tui      interactive local ops console (default on a TTY)
  beat     set beat mode / control URL override (independent knobs)
  configure  set beat mode, control override, and bandwidth caps
  doctor   local health checks (ports, decoy, disk, DNS)
  healthcheck  query the loopback health endpoint (container HEALTHCHECK)
  version  print version

Run "%[1]s <command> -h" for flags. Bare TTY invocation opens the TUI.
`

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func main() {
	if len(os.Args) < 2 {
		if isTTY(os.Stdin) && isTTY(os.Stdout) {
			os.Args = append(os.Args, "tui")
		} else {
			fmt.Fprintf(os.Stderr, usage, prog(), version)
			os.Exit(2)
		}
	}
	cmds := map[string]func(context.Context, []string) error{
		"init": cmdInit, "enroll": cmdEnroll, "register": cmdRegister, "run": cmdRun,
		"profile": cmdProfile, "donate": cmdDonate, "undonate": cmdUndonate,
		"status": cmdStatus, "tui": cmdTUI, "beat": cmdBeat, "configure": cmdConfigure,
		"doctor": cmdDoctor, "healthcheck": cmdHealthcheck,
		"version": func(context.Context, []string) error { fmt.Println(version); return nil },
	}
	run, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, usage, prog(), version)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		// Static prefix only; errors from the network can carry addresses.
		fmt.Fprintln(os.Stderr, prog()+":", err)
		os.Exit(1)
	}
}

// prog is the binary's own name: flag sets, usage, the stderr prefix, the
// default config path, the config env var and the metric name derive from it,
// so an install under another name carries no project name.
// Symlink aliases (e.g. dvrelay → dotvpn-relay) resolve to the real basename
// so status/doctor find /etc/dotvpn-relay and the systemd unit.
func prog() string {
	if p, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Base(real)
		}
		return filepath.Base(p)
	}
	return filepath.Base(os.Args[0])
}

// ident maps a program name to [a-z0-9_] for env var and metric names.
func ident(p string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return '_'
	}, p)
}

func envName(p string) string { return strings.ToUpper(ident(p)) + "_CONFIG" }

func metricName(p, suffix string) string {
	n := ident(p) + "_" + suffix
	if n[0] >= '0' && n[0] <= '9' {
		n = "_" + n
	}
	return n
}

func flags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(prog()+" "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs, fs.String("config", "", "config file (default: $"+envName(prog())+", /etc/"+prog()+"/config.yaml as root, else the user config dir)")
}

// configPath: -config, then $<PROG>_CONFIG, then $DOTVPN_RELAY_CONFIG (kept for
// existing installs), then /etc/<prog>/config.yaml as root, else the user config dir.
func configPath(v string) string {
	if v != "" {
		return v
	}
	for _, e := range []string{envName(prog()), "DOTVPN_RELAY_CONFIG"} {
		if v := os.Getenv(e); v != "" {
			return v
		}
	}
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		return "/etc/" + prog() + "/config.yaml"
	}
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, prog(), "config.yaml")
}

// readToken returns tok, or the first line of r when tok is "-", so the
// enrollment token need not appear in argv (`ps`, shell history, journald).
func readToken(tok string, r io.Reader) (string, error) {
	if tok != "-" {
		return tok, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, 4096))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func cmdInit(_ context.Context, args []string) error {
	fs, cfg := flags("init")
	listen := fs.String("listen", "", "public TCP listen address (default :443)")
	decoy := fs.String("decoy", "", "host:port of a real TLS server for everyone who is not a peer")
	cert := fs.String("tls-cert", "", "built-in TLS decoy certificate (instead of -decoy)")
	key := fs.String("tls-key", "", "built-in TLS decoy key")
	domain := fs.String("domain", "", "hostname (TLS SNI and DNS carrier zone when -dns)")
	dns := fs.Bool("dns", false, "enable DNS carrier on :53 (zone = -domain)")
	dnsListen := fs.String("dns-listen", "", "DNS carrier listen address (default :53 when -dns)")
	admin := fs.String("admin-listen", "", "loopback address for /healthz and /metrics (default 127.0.0.1:9101)")
	force := fs.Bool("force", false, "overwrite an existing config (regenerates the identity)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*cfg)
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s exists (use -force: the identity key would be regenerated)", path)
	}
	pub, priv, err := auth.GenerateKey()
	if err != nil {
		return err
	}
	dl := strings.TrimSpace(*dnsListen)
	if *dns && dl == "" {
		dl = ":53"
	}
	c := &config.Config{Listen: *listen, Decoy: *decoy, TLSCert: *cert, TLSKey: *key,
		Domain: strings.TrimSpace(*domain), DNSListen: dl, AdminListen: *admin, IdentityKey: auth.EncodeKey(priv)}
	c.Defaults()
	if err := c.Validate(); err != nil {
		return err
	}
	if err := config.Save(path, c); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	fmt.Println("identity public key:", auth.EncodeKey(pub))
	return nil
}

// cmdEnroll registers the relay's identity with control and stores control's
// public keys. Control URL override and beat mode are independent: curated
// enroll defaults to no override + beat disabled (ADR-012 seizure posture).
func cmdEnroll(ctx context.Context, args []string) error {
	fs, cfg := flags("enroll")
	ctrl := fs.String("control", defaultControlURL, "control public API URL (https; default "+defaultControlURL+")")
	token := fs.String("token", "", "one-time enrollment token from the core admin API; \"-\" reads it from stdin")
	pin := fs.String("pin", "", "base64 SHA-256 SPKI pin of control's TLS key, for a self-signed control")
	addr := fs.String("addr", "", "public listen host:port to match the pending relay on control")
	domain := fs.String("domain", "", "domain to match the pending relay on control")
	beatMode := fs.String("beat-mode", config.BeatDisabled, "beat mode: disabled|normal|conservative|telemetry (default disabled)")
	storeCtrl := fs.Bool("store-control", false, "persist -control as control_url override (default: use built-in default at beat time)")
	// Legacy -beat means normal mode + store control URL.
	legacyBeat := fs.Bool("beat", false, "legacy: same as -beat-mode normal -store-control")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*cfg)
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	u, err := parseControlURL(*ctrl)
	if err != nil {
		return err
	}
	tok, err := readToken(*token, os.Stdin)
	if err != nil {
		return err
	}
	if tok == "" {
		return errors.New("-token required")
	}
	priv, err := auth.DecodePrivateKey(c.IdentityKey)
	if err != nil {
		return errors.New("identity_key invalid: run `" + prog() + " init`")
	}
	matchAddr := *addr
	if matchAddr == "" {
		matchAddr = c.Listen
	}
	out, err := enroll(ctx, u, tok, *pin, priv.Public().(ed25519.PublicKey), matchAddr, *domain)
	if err != nil {
		return err
	}
	c.TrustedKeys = out.TrustedKeys
	c.RelayID = out.RelayID
	if s := strings.TrimSpace(*domain); s != "" {
		c.Domain = s
	} else if s := strings.TrimSpace(out.Domain); s != "" {
		c.Domain = s
	}
	if out.DNSCarrier && c.DNSListen == "" {
		c.DNSListen = ":53"
	}
	mode := config.NormalizeBeatMode(*beatMode)
	if *legacyBeat {
		mode = config.BeatNormal
		*storeCtrl = true
	}
	if err := config.ValidateBeatMode(mode); err != nil {
		return err
	}
	c.BeatMode = mode
	if *storeCtrl || *legacyBeat {
		c.ControlURL = u.String()
		c.ControlPin = *pin
	} else {
		// Seizure posture: no control URL on disk; runtime uses default when beating.
		c.ControlURL = ""
		c.ControlPin = ""
		if mode != config.BeatDisabled && u.String() != defaultControlURL {
			fmt.Println("warning: non-default -control used for enroll only; pass -store-control (or beat control -url) to beat that plane")
		}
	}
	if mode != config.BeatDisabled && !config.PoolLive(mode) {
		fmt.Println("warning: beat_mode", mode, "will not keep requires_beat relays in pools")
	}
	if mode == config.BeatNormal {
		fmt.Println("note: enable requires_beat on control if this curated relay must stay in pools")
	}
	if err := config.Save(path, c); err != nil {
		return err
	}
	fmt.Println("enrolled; trusted control keys:", len(out.TrustedKeys))
	fmt.Println("beat_mode:", c.BeatMode)
	resolved, _ := resolveControlURL(c.ControlURL)
	fmt.Println("control:  ", resolved.String(), "(override="+boolStr(c.ControlURL != "")+")")
	if out.RelayID != "" {
		fmt.Println("relay_id:", out.RelayID)
	}
	if out.Addr != "" {
		fmt.Println("addr:", out.Addr)
	}
	if out.Domain != "" {
		fmt.Println("domain:", out.Domain)
	}
	if out.DNSCarrier {
		fmt.Println("dns_carrier: true (dns_listen=:53)")
	}
	return nil
}

type enrollResult struct {
	RelayID     string
	TrustedKeys []string
	Addr        string
	Domain      string
	DNSCarrier  bool
}

func enroll(ctx context.Context, base *url.URL, token, pin string, pub ed25519.PublicKey, addr, domain string) (enrollResult, error) {
	body, _ := json.Marshal(map[string]string{
		"public_key": auth.EncodeKey(pub),
		"addr":       addr,
		"domain":     domain,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.JoinPath("/v1/relays/enroll").String(), bytes.NewReader(body))
	if err != nil {
		return enrollResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := controlClient(pin).Do(req)
	if err != nil {
		return enrollResult{}, errors.New("control unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		var er struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 512)).Decode(&er)
		if er.Error == "" {
			er.Error = "addr/domain does not match pending relay"
		}
		return enrollResult{}, fmt.Errorf("enrollment mismatch: %s", er.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return enrollResult{}, fmt.Errorf("enrollment refused (%d)", resp.StatusCode)
	}
	var out struct {
		RelayID     string   `json:"relay_id"`
		TrustedKeys []string `json:"trusted_keys"`
		Addr        string   `json:"addr"`
		Domain      string   `json:"domain"`
		DNSCarrier  bool     `json:"dns_carrier"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || len(out.TrustedKeys) == 0 {
		return enrollResult{}, errors.New("enrollment response invalid")
	}
	return enrollResult{
		RelayID: out.RelayID, TrustedKeys: out.TrustedKeys,
		Addr: out.Addr, Domain: out.Domain, DNSCarrier: out.DNSCarrier,
	}, nil
}

func controlClient(pin string) *http.Client {
	hc := &http.Client{Timeout: 20 * time.Second}
	if pin == "" {
		return hc
	}
	hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{
		// #nosec G402 -- VerifyConnection enforces the SPKI pin below.
		// codeql[go/disabled-certificate-check] SPKI pin checked in VerifyConnection for self-signed control
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || transport.SPKIPin(cs.PeerCertificates[0]) != pin {
				return errors.New("control key does not match -pin")
			}
			return nil
		},
	}}
	return hc
}

func parseControlURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("-control must be a domain or https URL")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + strings.TrimSuffix(raw, "/") + "/api"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !loopback.IsHost(u.Hostname())) {
		return nil, errors.New("-control must be a domain or https URL")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/api"
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}

// resolveControlURL returns the override if set, otherwise the production default.
func resolveControlURL(override string) (*url.URL, error) {
	raw := strings.TrimSpace(override)
	if raw == "" {
		raw = defaultControlURL
	}
	return parseControlURL(raw)
}

// cmdRegister self-registers a community relay without an admin token.
func cmdRegister(ctx context.Context, args []string) error {
	fs, cfg := flags("register")
	ctrl := fs.String("control", defaultControlURL, "control public API URL (https; default "+defaultControlURL+")")
	port := fs.Int("port", 443, "public TCP port control should dial")
	domain := fs.String("domain", "", "hostname clients and nodes present (TLS SNI)")
	dns := fs.Bool("dns", false, "advertise DNS carrier on UDP :53 (zone = -domain)")
	pin := fs.String("pin", "", "base64 SHA-256 SPKI pin of control's TLS key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*cfg)
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	u, err := parseControlURL(*ctrl)
	if err != nil {
		return err
	}
	dom := strings.TrimSpace(*domain)
	if dom == "" {
		return errors.New("-domain required")
	}
	priv, err := auth.DecodePrivateKey(c.IdentityKey)
	if err != nil {
		return errors.New("identity_key invalid: run `" + prog() + " init`")
	}
	pub := priv.Public().(ed25519.PublicKey)
	pubStr := auth.EncodeKey(pub)
	ts := time.Now().Unix()
	dnsFlag := 0
	if *dns {
		dnsFlag = 1
	}
	msg := []byte(fmt.Sprintf("register|%s|%d|%s|%d|%d", pubStr, *port, dom, dnsFlag, ts))
	sig := base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, msg))
	body, _ := json.Marshal(map[string]any{
		"public_key": pubStr, "port": *port, "domain": dom,
		"dns": *dns, "timestamp": ts, "signature": sig,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.JoinPath("/v1/relays/register").String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := controlClient(*pin).Do(req)
	if err != nil {
		return errors.New("control unreachable")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registration refused (%d)", resp.StatusCode)
	}
	var out struct {
		RelayID     string   `json:"relay_id"`
		Name        string   `json:"name"`
		Status      string   `json:"status"`
		TrustedKeys []string `json:"trusted_keys"`
		Invite      string   `json:"invite"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.RelayID == "" || len(out.TrustedKeys) == 0 {
		return errors.New("registration response invalid")
	}
	c.TrustedKeys = out.TrustedKeys
	c.RelayID = out.RelayID
	c.Name = out.Name
	c.ControlURL = u.String()
	c.ControlPin = *pin
	c.BeatMode = config.BeatNormal // community needs pool liveness
	c.Domain = dom
	if *dns {
		c.DNSListen = ":53"
	}
	if err := config.Save(path, c); err != nil {
		return err
	}
	fmt.Println("registered")
	fmt.Println("relay_id:", out.RelayID)
	fmt.Println("name:", out.Name)
	fmt.Println("status:", out.Status)
	fmt.Println("domain:", c.Domain)
	if c.DNSListen != "" {
		fmt.Println("dns_listen:", c.DNSListen)
	}
	fmt.Println("beat_mode:", c.BeatMode, "(required for community pool membership)")
	if out.Invite != "" {
		fmt.Println("relay link:", out.Invite)
	} else {
		fmt.Println("relay link:", relayInviteURI(out.RelayID))
	}
	return nil
}

func cmdRun(ctx context.Context, args []string) error {
	fs, cfg := flags("run")
	beatMode := fs.String("beat-mode", "", "override config beat_mode for this process (disabled|normal|conservative|telemetry)")
	noBeat := fs.Bool("no-beat", false, "legacy: same as -beat-mode disabled")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := config.Load(configPath(*cfg))
	if err != nil {
		return err
	}
	if *noBeat {
		c.BeatMode = config.BeatDisabled
	}
	if *beatMode != "" {
		if err := config.ValidateBeatMode(*beatMode); err != nil {
			return err
		}
		c.BeatMode = config.NormalizeBeatMode(*beatMode)
	}
	return serve(ctx, c)
}

func serve(ctx context.Context, c *config.Config) error {
	priv, err := auth.DecodePrivateKey(c.IdentityKey)
	if err != nil {
		return errors.New("identity_key invalid: run `" + prog() + " init`")
	}
	// Empty trusted_keys is allowed so a relay can listen (decoy + knock)
	// before register/enroll; pool auth rejects until keys are present.
	var pubs []ed25519.PublicKey
	for _, k := range c.TrustedKeys {
		p, err := auth.DecodePublicKey(k)
		if err != nil {
			return errors.New("trusted_keys invalid")
		}
		pubs = append(pubs, p)
	}
	// No access log: the default logger writes only static lifecycle lines.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var fallback func(net.Conn)
	if c.Decoy != "" {
		fallback = relay.ForwardTo(c.Decoy)
	} else {
		cert, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
		if err != nil {
			return errors.New("tls_cert/tls_key unreadable")
		}
		var stop func()
		fallback, stop = relay.Builtin(cert)
		defer stop()
	}
	srv, err := relay.New(relay.Options{
		Identity: priv, Verifier: auth.NewVerifier(pubs...), Fallback: fallback,
		MaxConns: c.MaxConns, MaxPoolPerNode: c.MaxPoolPerNode, MaxPool: c.MaxPool,
		Bandwidth: relay.BandwidthOpts{
			IngressMbps: c.BandwidthIngressMbps, EgressMbps: c.BandwidthEgressMbps,
			AvgWindow: c.AvgWindow(), AvgCapMbps: c.BandwidthAvgCapMbps,
			ThrottleMbps: c.BandwidthThrottleMbps,
		},
	})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	adm, err := loopback.Listen(c.AdminListen)
	if err != nil {
		return err
	}
	errc := make(chan error, 3)
	go func() { errc <- srv.ServeTCP(ctx, ln) }()
	if c.DNSListen != "" {
		pc, err := net.ListenPacket("udp", c.DNSListen)
		if err != nil {
			return err
		}
		go func() { errc <- srv.ServeDNS(ctx, pc, c.Domain) }()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		parked := metricName(prog(), "parked")
		active := metricName(prog(), "active")
		bytesIn := metricName(prog(), "bytes_in")
		bytesOut := metricName(prog(), "bytes_out")
		sessions := metricName(prog(), "sessions")
		inRate := metricName(prog(), "ingress_bps")
		outRate := metricName(prog(), "egress_bps")
		throttled := metricName(prog(), "bandwidth_throttled")
		fmt.Fprintf(w, "# TYPE %s gauge\n%s %d\n# TYPE %s gauge\n%s %d\n",
			parked, parked, srv.Parked(), active, active, srv.Active())
		fmt.Fprintf(w, "# TYPE %s counter\n%s %d\n# TYPE %s counter\n%s %d\n# TYPE %s counter\n%s %d\n",
			bytesIn, bytesIn, srv.BytesIn(), bytesOut, bytesOut, srv.BytesOut(),
			sessions, sessions, srv.Sessions())
		inBps, outBps := srv.BandwidthRates()
		th := 0
		if srv.BandwidthThrottled() {
			th = 1
		}
		fmt.Fprintf(w, "# TYPE %s gauge\n%s %d\n# TYPE %s gauge\n%s %d\n# TYPE %s gauge\n%s %d\n",
			inRate, inRate, inBps, outRate, outRate, outBps, throttled, throttled, th)
	})
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ErrorLog: slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)}
	go func() { <-ctx.Done(); _ = hs.Close() }()
	go func() { _ = hs.Serve(adm) }()
	base, jitter, beatOK := config.BeatInterval(c.BeatMode)
	if beatOK && c.RelayID != "" {
		if _, err := resolveControlURL(c.ControlURL); err != nil {
			return fmt.Errorf("control_url: %w", err)
		}
		if !config.PoolLive(c.BeatMode) {
			log.Info("beat mode is telemetry-only; will not keep requires_beat relays in pools", "mode", c.BeatMode)
		}
		go beatLoop(ctx, c, priv, srv, log, base, jitter)
	}
	log.Info("started", "beat_mode", c.BeatMode)
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

func beatLoop(ctx context.Context, c *config.Config, priv ed25519.PrivateKey, srv *relay.Server, log *slog.Logger, base, jitter time.Duration) {
	resolved, err := resolveControlURL(c.ControlURL)
	if err != nil {
		log.Warn("beat disabled: control URL invalid", "err", err)
		return
	}
	pub := auth.EncodeKey(priv.Public().(ed25519.PublicKey))
	interval := base
	if jitter > 0 {
		interval = base + time.Duration(rand.Int64N(int64(jitter)))
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	send := func() {
		ts := time.Now().Unix()
		parked := srv.Parked()
		active := srv.Active()
		bytesIn := int64(srv.BytesIn())
		bytesOut := int64(srv.BytesOut())
		sessions := int64(srv.Sessions())
		msg := []byte(fmt.Sprintf("beat|%s|%s|%d|%d|%d|%d|%d|%d",
			c.RelayID, version, parked, active, bytesIn, bytesOut, sessions, ts))
		sig := base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, msg))
		body, _ := json.Marshal(map[string]any{
			"relay_id": c.RelayID, "public_key": pub, "version": version,
			"parked": parked, "active": active,
			"bytes_in": bytesIn, "bytes_out": bytesOut, "sessions": sessions,
			"timestamp": ts, "signature": sig,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, resolved.JoinPath("/v1/relays/beat").String(), bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := controlClient(c.ControlPin).Do(req)
		if err != nil {
			log.Warn("beat failed", "err", err)
			return
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Warn("beat rejected", "status", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	send()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			send()
		}
	}
}

func cmdHealthcheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet(prog()+" healthcheck", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:9101", "admin address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+*addr+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: %s", resp.Status)
	}
	return nil
}
