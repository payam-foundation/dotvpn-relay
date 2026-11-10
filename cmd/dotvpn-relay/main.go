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
  run      serve
  status   local service status (loopback metrics + unit)
  doctor   local health checks (ports, decoy, disk, DNS)
  healthcheck  query the loopback health endpoint (container HEALTHCHECK)
  version  print version

Run "%[1]s <command> -h" for flags. Bare TTY invocation runs status.
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
			os.Args = append(os.Args, "status")
		} else {
			fmt.Fprintf(os.Stderr, usage, prog(), version)
			os.Exit(2)
		}
	}
	cmds := map[string]func(context.Context, []string) error{
		"init": cmdInit, "enroll": cmdEnroll, "register": cmdRegister, "run": cmdRun,
		"status": cmdStatus, "doctor": cmdDoctor, "healthcheck": cmdHealthcheck,
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
func prog() string { return filepath.Base(os.Args[0]) }

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
	dnsListen := fs.String("dns-listen", "", "enable the DNS-shaped UDP carrier on this address (e.g. :53)")
	dnsZone := fs.String("dns-zone", "", "zone the DNS carrier answers for")
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
	c := &config.Config{Listen: *listen, Decoy: *decoy, TLSCert: *cert, TLSKey: *key,
		DNSListen: *dnsListen, DNSZone: *dnsZone, AdminListen: *admin, IdentityKey: auth.EncodeKey(priv)}
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
// public keys. The control URL is used once and never written to disk, so a
// seized relay does not point at control.
func cmdEnroll(ctx context.Context, args []string) error {
	fs, cfg := flags("enroll")
	ctrl := fs.String("control", defaultControlURL, "control public API URL (https; default "+defaultControlURL+")")
	token := fs.String("token", "", "one-time enrollment token from the core admin API; \"-\" reads it from stdin")
	pin := fs.String("pin", "", "base64 SHA-256 SPKI pin of control's TLS key, for a self-signed control")
	addr := fs.String("addr", "", "public listen host:port to match the pending relay on control")
	sni := fs.String("sni", "", "domain/SNI to match the pending relay on control")
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
	out, err := enroll(ctx, u, tok, *pin, priv.Public().(ed25519.PublicKey), matchAddr, *sni)
	if err != nil {
		return err
	}
	c.TrustedKeys = out.TrustedKeys
	c.RelayID = out.RelayID
	// Admin enroll never stores control's URL or pin (ADR-012 seizure posture).
	c.ControlURL = ""
	c.ControlPin = ""
	if err := config.Save(path, c); err != nil {
		return err
	}
	fmt.Println("enrolled; trusted control keys:", len(out.TrustedKeys))
	if out.RelayID != "" {
		fmt.Println("relay_id:", out.RelayID)
	}
	if out.Addr != "" {
		fmt.Println("addr:", out.Addr)
	}
	if out.SNI != "" {
		fmt.Println("sni:", out.SNI)
	}
	if out.DNSZone != "" {
		fmt.Println("dns_zone:", out.DNSZone)
	}
	return nil
}

type enrollResult struct {
	RelayID     string
	TrustedKeys []string
	Addr        string
	SNI         string
	DNSAddr     string
	DNSZone     string
}

func enroll(ctx context.Context, base *url.URL, token, pin string, pub ed25519.PublicKey, addr, sni string) (enrollResult, error) {
	body, _ := json.Marshal(map[string]string{
		"public_key": auth.EncodeKey(pub),
		"addr":       addr,
		"sni":        sni,
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
			er.Error = "addr/sni does not match pending relay"
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
		SNI         string   `json:"sni"`
		DNSAddr     string   `json:"dns_addr"`
		DNSZone     string   `json:"dns_zone"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || len(out.TrustedKeys) == 0 {
		return enrollResult{}, errors.New("enrollment response invalid")
	}
	return enrollResult{
		RelayID: out.RelayID, TrustedKeys: out.TrustedKeys,
		Addr: out.Addr, SNI: out.SNI, DNSAddr: out.DNSAddr, DNSZone: out.DNSZone,
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

// cmdRegister self-registers a community relay without an admin token.
func cmdRegister(ctx context.Context, args []string) error {
	fs, cfg := flags("register")
	ctrl := fs.String("control", defaultControlURL, "control public API URL (https; default "+defaultControlURL+")")
	port := fs.Int("port", 443, "public TCP port control should dial")
	sni := fs.String("sni", "", "SNI clients and nodes present")
	dnsPort := fs.Int("dns-port", 0, "optional DNS-shaped UDP port")
	dnsZone := fs.String("dns-zone", "", "zone for the DNS carrier")
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
	if strings.TrimSpace(*sni) == "" {
		return errors.New("-sni required")
	}
	if (*dnsPort == 0) != (*dnsZone == "") {
		return errors.New("-dns-port and -dns-zone go together")
	}
	priv, err := auth.DecodePrivateKey(c.IdentityKey)
	if err != nil {
		return errors.New("identity_key invalid: run `" + prog() + " init`")
	}
	pub := priv.Public().(ed25519.PublicKey)
	pubStr := auth.EncodeKey(pub)
	ts := time.Now().Unix()
	msg := []byte(fmt.Sprintf("register|%s|%d|%s|%d|%s|%d", pubStr, *port, *sni, *dnsPort, *dnsZone, ts))
	sig := base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, msg))
	body, _ := json.Marshal(map[string]any{
		"public_key": pubStr, "port": *port, "sni": *sni,
		"dns_port": *dnsPort, "dns_zone": *dnsZone, "timestamp": ts, "signature": sig,
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
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.RelayID == "" || len(out.TrustedKeys) == 0 {
		return errors.New("registration response invalid")
	}
	c.TrustedKeys = out.TrustedKeys
	c.RelayID = out.RelayID
	c.Name = out.Name
	c.ControlURL = u.String()
	c.ControlPin = *pin
	if err := config.Save(path, c); err != nil {
		return err
	}
	fmt.Println("registered")
	fmt.Println("relay_id:", out.RelayID)
	fmt.Println("name:", out.Name)
	fmt.Println("status:", out.Status)
	fmt.Println("relay link: dotvpn-relay://" + out.RelayID)
	return nil
}

func cmdRun(ctx context.Context, args []string) error {
	fs, cfg := flags("run")
	noBeat := fs.Bool("no-beat", false, "do not send telemetry beats to control")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := config.Load(configPath(*cfg))
	if err != nil {
		return err
	}
	return serve(ctx, c, !*noBeat)
}

func serve(ctx context.Context, c *config.Config, beat bool) error {
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
	srv, err := relay.New(relay.Options{Identity: priv, Verifier: auth.NewVerifier(pubs...), Fallback: fallback, MaxConns: c.MaxConns})
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
		go func() { errc <- srv.ServeDNS(ctx, pc, c.DNSZone) }()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		parked := metricName(prog(), "parked")
		active := metricName(prog(), "active")
		fmt.Fprintf(w, "# TYPE %s gauge\n%s %d\n# TYPE %s gauge\n%s %d\n",
			parked, parked, srv.Parked(), active, active, srv.Active())
	})
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ErrorLog: slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)}
	go func() { <-ctx.Done(); _ = hs.Close() }()
	go func() { _ = hs.Serve(adm) }()
	if beat && c.ControlURL != "" && c.RelayID != "" {
		if _, err := parseControlURL(c.ControlURL); err != nil {
			return fmt.Errorf("control_url: %w", err)
		}
		go beatLoop(ctx, c, priv, srv, log)
	}
	log.Info("started")
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

func beatLoop(ctx context.Context, c *config.Config, priv ed25519.PrivateKey, srv *relay.Server, log *slog.Logger) {
	base, err := parseControlURL(c.ControlURL)
	if err != nil {
		log.Warn("beat disabled: control_url invalid", "err", err)
		return
	}
	pub := auth.EncodeKey(priv.Public().(ed25519.PublicKey))
	interval := 30*time.Second + time.Duration(rand.Int64N(int64(15*time.Second)))
	t := time.NewTicker(interval)
	defer t.Stop()
	send := func() {
		ts := time.Now().Unix()
		parked := srv.Parked()
		msg := []byte(fmt.Sprintf("beat|%s|%s|%d|%d", c.RelayID, version, parked, ts))
		sig := base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, msg))
		body, _ := json.Marshal(map[string]any{
			"relay_id": c.RelayID, "public_key": pub, "version": version,
			"parked": parked, "timestamp": ts, "signature": sig,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.JoinPath("/v1/relays/beat").String(), bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := controlClient(c.ControlPin).Do(req)
		if err != nil {
			log.Debug("beat failed")
			return
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
