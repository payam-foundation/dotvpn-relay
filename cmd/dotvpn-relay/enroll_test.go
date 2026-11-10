package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/payam-foundation/dotvpn-relay/internal/config"
	"github.com/payam-foundation/dotvpn-wire/auth"
	"github.com/payam-foundation/dotvpn-wire/transport"
)

func TestEnrollPin(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"relay_id": "r1", "trusted_keys": []string{"k"}})
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	good := transport.SPKIPin(srv.Certificate())

	if _, err := enroll(context.Background(), u, "t", "", pub, "", ""); err == nil {
		t.Fatal("self-signed control accepted without pin")
	}
	if _, err := enroll(context.Background(), u, "t", "AAAA", pub, "", ""); err == nil {
		t.Fatal("wrong pin accepted")
	}
	out, err := enroll(context.Background(), u, "t", good, pub, "1.2.3.4:443", "relay.example")
	if err != nil || len(out.TrustedKeys) != 1 || out.RelayID != "r1" {
		t.Fatalf("good pin refused: %+v %v", out, err)
	}
}

func TestRegisterProofOfPossession(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/relays/register" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "no", 404)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"relay_id": "rid", "name": "quiet-grove-abcd", "status": "active",
			"trusted_keys": []string{"ctl"},
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	privBytes := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(privBytes); err != nil {
		t.Fatal(err)
	}
	priv := ed25519.NewKeyFromSeed(privBytes)
	pub := priv.Public().(ed25519.PublicKey)
	if err := os.WriteFile(cfgPath, []byte("listen: 127.0.0.1:0\ndecoy: 127.0.0.1:1\nidentity_key: "+
		auth.EncodeKey(priv)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	pin := transport.SPKIPin(srv.Certificate())
	args := []string{
		"-config", cfgPath, "-control", srv.URL, "-sni", "cdn.example.com",
		"-port", "443", "-pin", pin,
	}
	if err := cmdRegister(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if gotBody["sni"] != "cdn.example.com" || gotBody["public_key"] != auth.EncodeKey(pub) {
		t.Fatalf("register body: %#v", gotBody)
	}
	sig, _ := gotBody["signature"].(string)
	ts := int64(gotBody["timestamp"].(float64))
	msg := []byte(fmt.Sprintf("register|%s|%d|%s|%d|%s|%d", auth.EncodeKey(pub), 443, "cdn.example.com", 0, "", ts))
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !ed25519.Verify(pub, msg, raw) {
		t.Fatalf("signature does not match core registerMessage: %v", err)
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	wantCtrl, err := parseControlURL(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if c.RelayID != "rid" || c.Name != "quiet-grove-abcd" || c.ControlURL != wantCtrl.String() || c.ControlPin != pin {
		t.Fatalf("config not updated: %+v", c)
	}
	if len(c.TrustedKeys) != 1 || c.TrustedKeys[0] != "ctl" {
		t.Fatalf("trusted_keys: %v", c.TrustedKeys)
	}
}

func TestNamesFollowProgramName(t *testing.T) {
	for _, c := range []struct{ prog, env, metric string }{
		{"dotvpn-relay", "DOTVPN_RELAY_CONFIG", "dotvpn_relay_parked"},
		{"quiet-fox-3a9f", "QUIET_FOX_3A9F_CONFIG", "quiet_fox_3a9f_parked"},
		{"9lives", "9LIVES_CONFIG", "_9lives_parked"},
	} {
		if got := envName(c.prog); got != c.env {
			t.Errorf("envName(%q) = %q, want %q", c.prog, got, c.env)
		}
		if got := metricName(c.prog, "parked"); got != c.metric {
			t.Errorf("metricName(%q) = %q, want %q", c.prog, got, c.metric)
		}
	}
}

func TestConfigPathEnv(t *testing.T) {
	t.Setenv("DOTVPN_RELAY_CONFIG", "")
	t.Setenv(envName(prog()), "")
	if got := configPath("/x/flag.yaml"); got != "/x/flag.yaml" {
		t.Fatalf("flag lost: %q", got)
	}
	t.Setenv("DOTVPN_RELAY_CONFIG", "/x/legacy.yaml")
	if got := configPath(""); got != "/x/legacy.yaml" {
		t.Fatalf("legacy env ignored: %q", got)
	}
	t.Setenv(envName(prog()), "/x/neutral.yaml")
	if got := configPath(""); got != "/x/neutral.yaml" {
		t.Fatalf("program env ignored: %q", got)
	}
}

func TestReadToken(t *testing.T) {
	if got, err := readToken("abc", strings.NewReader("zzz")); err != nil || got != "abc" {
		t.Fatalf("literal token: %q %v", got, err)
	}
	if got, err := readToken("-", strings.NewReader("s3cret\n")); err != nil || got != "s3cret" {
		t.Fatalf("stdin token: %q %v", got, err)
	}
}

func TestDefaultControlURL(t *testing.T) {
	if defaultControlURL != "https://ctrl.dotvpn.org/api" {
		t.Fatalf("defaultControlURL = %q", defaultControlURL)
	}
	u, err := parseControlURL(defaultControlURL)
	if err != nil || u.Host != "ctrl.dotvpn.org" || u.Path != "/api" {
		t.Fatalf("parse default: %v %v", u, err)
	}
	u, err = parseControlURL("ctrl.dotvpn.org")
	if err != nil || u.String() != "https://ctrl.dotvpn.org/api" {
		t.Fatalf("parse domain: %v %v", u, err)
	}
}
