package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/payam-foundation/dotvpn-wire/transport"
)

func TestEnrollPin(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string][]string{"trusted_keys": {"k"}})
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	good := transport.SPKIPin(srv.Certificate())

	if _, err := enroll(context.Background(), u, "t", "", pub); err == nil {
		t.Fatal("self-signed control accepted without pin")
	}
	if _, err := enroll(context.Background(), u, "t", "AAAA", pub); err == nil {
		t.Fatal("wrong pin accepted")
	}
	if k, err := enroll(context.Background(), u, "t", good, pub); err != nil || len(k) != 1 {
		t.Fatalf("good pin refused: %v", err)
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
		if got := metricName(c.prog); got != c.metric {
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
