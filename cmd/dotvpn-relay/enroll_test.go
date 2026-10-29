package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
