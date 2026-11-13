// Package config is the relay's YAML file. It holds the relay's own identity
// key and control's public keys; no node address, no client data.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/payam-foundation/dotvpn-wire/atomicfile"
	"github.com/payam-foundation/dotvpn-wire/loopback"
)

// Beat modes. Independent of ControlURL: empty ControlURL resolves to the
// production default at runtime when a mode other than disabled needs control.
const (
	BeatDisabled     = "disabled"
	BeatNormal       = "normal"       // ~45–75s; keeps requires_beat relays live
	BeatConservative = "conservative" // 1h; telemetry only
	BeatTelemetry    = "telemetry"    // 12h; telemetry only
)

// Config is the relay's YAML file.
type Config struct {
	// Listen is the public TCP address. Default ":443".
	Listen string `yaml:"listen"`
	// Domain is the hostname used as TLS SNI on :443 and as the DNS carrier
	// zone when DNSListen is set. Set by enroll/register from control (or -domain).
	Domain string `yaml:"domain,omitempty"`
	// DNSListen enables the DNS-shaped UDP carrier (e.g. ":53"); the zone is Domain.
	DNSListen string `yaml:"dns_listen"`
	// IdentityKey is the relay's base64url Ed25519 private key.
	IdentityKey string `yaml:"identity_key"`
	// TrustedKeys are control's public keys; set by `enroll` or `register`.
	TrustedKeys []string `yaml:"trusted_keys"`
	// RelayID is assigned by control. Name is set on open registration.
	RelayID string `yaml:"relay_id,omitempty"`
	Name    string `yaml:"name,omitempty"`
	// ControlURL is an optional override of the control plane API. Empty means
	// use the built-in default (ctrl.dotvpn.org/api) when beats or profile need
	// it. It is not a beat switch — see BeatMode.
	ControlURL string `yaml:"control_url,omitempty"`
	ControlPin string `yaml:"control_pin,omitempty"`
	// BeatMode is disabled|normal|conservative|telemetry. Default disabled.
	// Legacy configs with control_url set and beat_mode omitted migrate to normal.
	BeatMode string `yaml:"beat_mode,omitempty"`
	// Decoy is host:port of a real TLS server that answers everything that is
	// not a dotvpn peer (a stock nginx on this host is the best choice).
	// Alternatively TLSCert and TLSKey enable a built-in TLS web server.
	Decoy   string `yaml:"decoy"`
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`
	// AdminListen serves /healthz and /metrics. Loopback or unix socket only.
	AdminListen string `yaml:"admin_listen"`
	MaxConns    int    `yaml:"max_conns"`
	// MaxPoolPerNode / MaxPool bound parked reverse tunnels (0 = package defaults 16 / 1024).
	MaxPoolPerNode int `yaml:"max_pool_per_node"`
	MaxPool        int `yaml:"max_pool"`
	// BandwidthIngressMbps / BandwidthEgressMbps cap splice traffic (0 = unlimited).
	// Ingress = peer→relay (toward node); egress = relay→peer (from node).
	BandwidthIngressMbps float64 `yaml:"bandwidth_ingress_mbps,omitempty"`
	BandwidthEgressMbps  float64 `yaml:"bandwidth_egress_mbps,omitempty"`
	// Optional rolling-average soft cap: when average Mbps over BandwidthAvgWindow
	// exceeds BandwidthAvgCapMbps, sustained rates are limited to BandwidthThrottleMbps.
	BandwidthAvgWindow    string  `yaml:"bandwidth_avg_window,omitempty"`
	BandwidthAvgCapMbps   float64 `yaml:"bandwidth_avg_cap_mbps,omitempty"`
	BandwidthThrottleMbps float64 `yaml:"bandwidth_throttle_mbps,omitempty"`
}

// Defaults fills unset fields without overwriting user values, then migrates
// legacy beat-via-control_url configs.
func (c *Config) Defaults() {
	if c.Listen == "" {
		c.Listen = ":443"
	}
	if c.AdminListen == "" {
		c.AdminListen = "127.0.0.1:9101"
	}
	c.migrateBeatMode()
	if c.BeatMode == "" {
		c.BeatMode = BeatDisabled
	}
}

func (c *Config) migrateBeatMode() {
	if c.BeatMode != "" {
		return
	}
	// Pre-beat_mode: a stored control_url meant beats were enabled.
	if strings.TrimSpace(c.ControlURL) != "" {
		c.BeatMode = BeatNormal
	}
}

// CheckDomain is the hostname used for live DNS/TLS checks.
func (c *Config) CheckDomain() string {
	return strings.TrimSpace(c.Domain)
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
	if c.DNSListen != "" && strings.TrimSpace(c.Domain) == "" {
		return errors.New("dns_listen requires domain (carrier zone)")
	}
	if c.MaxConns < 0 {
		return errors.New("max_conns must be >= 0 (0 keeps the default)")
	}
	if c.MaxPoolPerNode < 0 {
		return errors.New("max_pool_per_node must be >= 0 (0 keeps the default)")
	}
	if c.MaxPool < 0 {
		return errors.New("max_pool must be >= 0 (0 keeps the default)")
	}
	if err := ValidateBeatMode(c.BeatMode); err != nil {
		return err
	}
	if c.BandwidthIngressMbps < 0 || c.BandwidthEgressMbps < 0 {
		return errors.New("bandwidth_*_mbps must be >= 0 (0 = unlimited)")
	}
	if c.BandwidthAvgCapMbps < 0 || c.BandwidthThrottleMbps < 0 {
		return errors.New("bandwidth_avg_cap_mbps and bandwidth_throttle_mbps must be >= 0")
	}
	if c.BandwidthAvgWindow != "" {
		d, err := time.ParseDuration(c.BandwidthAvgWindow)
		if err != nil || d <= 0 {
			return errors.New("bandwidth_avg_window must be a positive duration (e.g. 24h)")
		}
		if c.BandwidthAvgCapMbps <= 0 || c.BandwidthThrottleMbps <= 0 {
			return errors.New("bandwidth_avg_window requires bandwidth_avg_cap_mbps and bandwidth_throttle_mbps > 0")
		}
	} else if c.BandwidthAvgCapMbps > 0 || c.BandwidthThrottleMbps > 0 {
		return errors.New("bandwidth_avg_cap_mbps/throttle require bandwidth_avg_window")
	}
	return nil
}

// ValidateBeatMode accepts the known modes (empty is allowed before Defaults).
func ValidateBeatMode(mode string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", BeatDisabled, BeatNormal, BeatConservative, BeatTelemetry:
		return nil
	default:
		return fmt.Errorf("beat_mode must be %s, %s, %s, or %s",
			BeatDisabled, BeatNormal, BeatConservative, BeatTelemetry)
	}
}

// NormalizeBeatMode returns the canonical mode string.
func NormalizeBeatMode(mode string) string {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		return BeatDisabled
	}
	return m
}

// BeatInterval returns the base interval and jitter window for mode.
// Normal stays under core's 90s stale window. Conservative/telemetry are
// telemetry-only and will not keep requires_beat relays in pools.
func BeatInterval(mode string) (base, jitter time.Duration, ok bool) {
	switch NormalizeBeatMode(mode) {
	case BeatNormal:
		return 45 * time.Second, 30 * time.Second, true
	case BeatConservative:
		return time.Hour, 5 * time.Minute, true
	case BeatTelemetry:
		return 12 * time.Hour, time.Hour, true
	default:
		return 0, 0, false
	}
}

// PoolLive reports whether mode is intended to keep requires_beat relays live.
func PoolLive(mode string) bool {
	return NormalizeBeatMode(mode) == BeatNormal
}

// AvgWindow parses BandwidthAvgWindow or returns 0 when unset.
func (c *Config) AvgWindow() time.Duration {
	if c.BandwidthAvgWindow == "" {
		return 0
	}
	d, err := time.ParseDuration(c.BandwidthAvgWindow)
	if err != nil {
		return 0
	}
	return d
}

// removedKeys are top-level keys that earlier releases wrote and that no
// longer exist. Load ignores them so an upgrade never fails to start; any
// other unknown key is still an error.
var removedKeys = []string{"dns_zone", "sni"}

func dropRemovedKeys(b []byte) ([]byte, error) {
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil || m == nil {
		return b, nil // let the strict decoder report the real error
	}
	found := false
	for _, k := range removedKeys {
		if _, ok := m[k]; ok {
			delete(m, k)
			found = true
		}
	}
	if !found {
		return b, nil
	}
	return yaml.Marshal(m)
}

// Load reads, defaults and validates path; unknown keys are an error.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b, err = dropRemovedKeys(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Defaults()
	c.BeatMode = NormalizeBeatMode(c.BeatMode)
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Save writes atomically, then sets mode 0640 (group-readable so a service
// user can read a root-owned file; never world-readable). When path already
// exists, uid/gid are preserved so root CLI/TUI edits do not lock out the
// service account.
func Save(path string, c *Config) error {
	c.BeatMode = NormalizeBeatMode(c.BeatMode)
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	var uid, gid int = -1, -1
	if st, err := os.Stat(path); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(sys.Uid), int(sys.Gid)
		}
	}
	if err := atomicfile.Write(path, b, 0o750); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o640); err != nil {
		return err
	}
	if uid >= 0 && gid >= 0 {
		_ = os.Chown(path, uid, gid)
	}
	return nil
}
