package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateBeatModeFromControlURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	raw := "listen: \":443\"\ndecoy: 127.0.0.1:8443\nidentity_key: x\ncontrol_url: https://ctrl.dotvpn.org/api\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	// identity_key "x" fails Decode later; Load still validates decoy etc.
	// Use a minimal valid decoy-only file — identity not validated in config.
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.BeatMode != BeatNormal {
		t.Fatalf("beat_mode = %q, want normal (migrated from control_url)", c.BeatMode)
	}
}

func TestDefaultBeatDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("listen: \":443\"\ndecoy: 127.0.0.1:8443\nidentity_key: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.BeatMode != BeatDisabled {
		t.Fatalf("beat_mode = %q, want disabled", c.BeatMode)
	}
}

func TestBeatInterval(t *testing.T) {
	base, jitter, ok := BeatInterval(BeatNormal)
	if !ok || base != 45*time.Second || jitter != 30*time.Second {
		t.Fatalf("normal: %v %v %v", base, jitter, ok)
	}
	if base+jitter >= 90*time.Second {
		t.Fatal("normal interval must stay under 90s stale window")
	}
	base, jitter, ok = BeatInterval(BeatConservative)
	if !ok || base != time.Hour {
		t.Fatalf("conservative: %v %v %v", base, jitter, ok)
	}
	base, _, ok = BeatInterval(BeatTelemetry)
	if !ok || base != 12*time.Hour {
		t.Fatalf("telemetry: %v %v", base, ok)
	}
	if _, _, ok := BeatInterval(BeatDisabled); ok {
		t.Fatal("disabled should not beat")
	}
}

func TestDomainAndDNSListen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	raw := "listen: \":443\"\ndecoy: 127.0.0.1:8443\nidentity_key: x\ndomain: relay.example.com\ndns_listen: \":53\"\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Domain != "relay.example.com" || c.CheckDomain() != "relay.example.com" {
		t.Fatalf("domain=%q CheckDomain=%q", c.Domain, c.CheckDomain())
	}
	if c.DNSListen != ":53" {
		t.Fatalf("dns_listen=%q", c.DNSListen)
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("listen: \":443\"\ndecoy: 127.0.0.1:8443\nidentity_key: x\ndns_listen: \":53\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatal("expected error when dns_listen set without domain")
	}
}

func TestBandwidthAvgValidation(t *testing.T) {
	c := &Config{
		Listen: ":443", Decoy: "127.0.0.1:1", AdminListen: "127.0.0.1:9101",
		BeatMode: BeatDisabled, BandwidthAvgWindow: "24h", BandwidthAvgCapMbps: 100,
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error when throttle missing")
	}
	c.BandwidthThrottleMbps = 100
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadIgnoresRemovedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	raw := "listen: \":443\"\ndecoy: 127.0.0.1:8443\nidentity_key: x\ndns_zone: z.example.com\nsni: a.example.com\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("removed keys must not fail Load: %v", err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("listen: \":443\"\ndecoy: 127.0.0.1:8443\nidentity_key: x\nbogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatal("unknown keys must still fail Load")
	}
}
