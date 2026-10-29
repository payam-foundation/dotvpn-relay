// dotvpn-relay is the in-country stealth hop. It has no client or admin code.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/payam-foundation/dotvpn-relay/internal/config"
	"github.com/payam-foundation/dotvpn-relay/internal/relay"
	"github.com/payam-foundation/dotvpn-wire/auth"
	"github.com/payam-foundation/dotvpn-wire/loopback"
	"github.com/payam-foundation/dotvpn-wire/transport"
)

var version = "dev"

const usage = `dotvpn-relay %s

  init     write a config with a fresh identity key
  enroll   register this relay with control (one-time enrollment token)
  run      serve
  healthcheck  query the loopback health endpoint (container HEALTHCHECK)
  version  print version

Run "dotvpn-relay <command> -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	cmds := map[string]func(context.Context, []string) error{
		"init": cmdInit, "enroll": cmdEnroll, "run": cmdRun, "healthcheck": cmdHealthcheck,
		"version": func(context.Context, []string) error { fmt.Println(version); return nil },
	}
	run, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		// Static prefix only; errors from the network can carry addresses.
		fmt.Fprintln(os.Stderr, "dotvpn-relay:", err)
		os.Exit(1)
	}
}

func flags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("dotvpn-relay "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs, fs.String("config", "", "config file (default: $DOTVPN_RELAY_CONFIG, /etc/dotvpn-relay/config.yaml as root, else the user config dir)")
}

func configPath(v string) string {
	if v != "" {
		return v
	}
	if v := os.Getenv("DOTVPN_RELAY_CONFIG"); v != "" {
		return v
	}
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		return "/etc/dotvpn-relay/config.yaml"
	}
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, "dotvpn-relay", "config.yaml")
}

func cmdInit(_ context.Context, args []string) error {
	fs, cfg := flags("init")
	listen := fs.String("listen", "", "public TCP listen address (default :443)")
	decoy := fs.String("decoy", "", "host:port of a real TLS server for non-dotvpn visitors")
	cert := fs.String("tls-cert", "", "built-in TLS decoy certificate (instead of -decoy)")
	key := fs.String("tls-key", "", "built-in TLS decoy key")
	dnsListen := fs.String("dns-listen", "", "enable the DNS-shaped UDP carrier on this address (e.g. :53)")
	dnsZone := fs.String("dns-zone", "", "zone the DNS carrier answers for")
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
		DNSListen: *dnsListen, DNSZone: *dnsZone, IdentityKey: auth.EncodeKey(priv)}
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
	ctrl := fs.String("control", "", "control public API URL (https)")
	token := fs.String("token", "", "one-time enrollment token from the core admin API")
	pin := fs.String("pin", "", "base64 SHA-256 SPKI pin of control's TLS key, for a self-signed control")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := configPath(*cfg)
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	u, err := url.Parse(*ctrl)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !loopback.IsHost(u.Hostname())) {
		return errors.New("-control must be an https URL")
	}
	if *token == "" {
		return errors.New("-token required")
	}
	priv, err := auth.DecodePrivateKey(c.IdentityKey)
	if err != nil {
		return errors.New("identity_key invalid: run `dotvpn-relay init`")
	}
	keys, err := enroll(ctx, u, *token, *pin, priv.Public().(ed25519.PublicKey))
	if err != nil {
		return err
	}
	c.TrustedKeys = keys
	if err := config.Save(path, c); err != nil {
		return err
	}
	fmt.Println("enrolled; trusted control keys:", len(keys))
	return nil
}

func enroll(ctx context.Context, base *url.URL, token, pin string, pub ed25519.PublicKey) ([]string, error) {
	body, _ := json.Marshal(map[string]string{"public_key": auth.EncodeKey(pub)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.JoinPath("/v1/relays/enroll").String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 20 * time.Second}
	if pin != "" {
		// Pinned self-signed control: skip chain verification, check the key instead.
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // #nosec G402 -- VerifyConnection enforces the pin
			MinVersion:         tls.VersionTLS12,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 || transport.SPKIPin(cs.PeerCertificates[0]) != pin {
					return errors.New("control key does not match -pin")
				}
				return nil
			},
		}}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, errors.New("control unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("enrollment refused (%d)", resp.StatusCode)
	}
	var out struct {
		TrustedKeys []string `json:"trusted_keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || len(out.TrustedKeys) == 0 {
		return nil, errors.New("enrollment response invalid")
	}
	return out.TrustedKeys, nil
}

func cmdRun(ctx context.Context, args []string) error {
	fs, cfg := flags("run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := config.Load(configPath(*cfg))
	if err != nil {
		return err
	}
	return serve(ctx, c)
}

func serve(ctx context.Context, c *config.Config) error {
	priv, err := auth.DecodePrivateKey(c.IdentityKey)
	if err != nil {
		return errors.New("identity_key invalid: run `dotvpn-relay init`")
	}
	if len(c.TrustedKeys) == 0 {
		return errors.New("not enrolled: run `dotvpn-relay enroll`")
	}
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
		fmt.Fprintf(w, "# TYPE dotvpn_relay_parked gauge\ndotvpn_relay_parked %d\n", srv.Parked())
	})
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ErrorLog: slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)}
	go func() { <-ctx.Done(); _ = hs.Close() }()
	go func() { _ = hs.Serve(adm) }()
	log.Info("relay started")
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

func cmdHealthcheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dotvpn-relay healthcheck", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:9101", "relay admin address")
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
