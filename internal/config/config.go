// Package config is the relay's YAML file. It holds the relay's own identity
// key and control's public keys; no node address, no client data.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/payam-foundation/dotvpn-wire/atomicfile"
	"github.com/payam-foundation/dotvpn-wire/loopback"
)

// Config is the relay's YAML file.
type Config struct {
	// Listen is the public TCP address. Default ":443".
	Listen string `yaml:"listen"`
	// DNSListen enables the DNS-shaped UDP carrier (e.g. ":53"); DNSZone is
	// the zone it answers for.
	DNSListen string `yaml:"dns_listen"`
	DNSZone   string `yaml:"dns_zone"`
	// IdentityKey is the relay's base64url Ed25519 private key.
	IdentityKey string `yaml:"identity_key"`
	// TrustedKeys are control's public keys; set by `enroll`.
	TrustedKeys []string `yaml:"trusted_keys"`
	// Decoy is host:port of a real TLS server that answers everything that is
	// not a dotvpn peer (a stock nginx on this host is the best choice).
	// Alternatively TLSCert and TLSKey enable a built-in TLS web server.
	Decoy   string `yaml:"decoy"`
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`
	// AdminListen serves /healthz and /metrics. Loopback or unix socket only.
	AdminListen string `yaml:"admin_listen"`
	MaxConns    int    `yaml:"max_conns"`
}

// Defaults fills unset fields without overwriting user values.
func (c *Config) Defaults() {
	if c.Listen == "" {
		c.Listen = ":443"
	}
	if c.AdminListen == "" {
		c.AdminListen = "127.0.0.1:9101"
	}
}

// Validate fails at startup on an exposed admin address or inconsistent decoy/TLS/DNS settings.
func (c *Config) Validate() error {
	if err := loopback.Check(c.AdminListen); err != nil {
		return fmt.Errorf("admin_listen: %w", err)
	}
	if (c.Decoy == "") == (c.TLSCert == "") {
		return errors.New("set exactly one of decoy (host:port of a real TLS server) or tls_cert+tls_key")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("tls_cert and tls_key go together")
	}
	if (c.DNSListen == "") != (c.DNSZone == "") {
		return errors.New("dns_listen and dns_zone go together")
	}
	return nil
}

// Load reads, defaults and validates path; unknown keys are an error.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Defaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Save writes atomically with mode 0600.
func Save(path string, c *Config) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, b, 0o750)
}
