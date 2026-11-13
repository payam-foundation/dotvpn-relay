package main

import (
	"strings"
	"testing"
)

func TestStatusInviteURI(t *testing.T) {
	if got := relayInviteURI("relay-123"); got != "dvr://relay-123" {
		t.Fatalf("invite = %q", got)
	}
}

func TestParseMetrics(t *testing.T) {
	body := `# TYPE dotvpn_relay_parked gauge
dotvpn_relay_parked 3
# TYPE dotvpn_relay_active gauge
dotvpn_relay_active 1
`
	p, a, bi, bo, sess, err := parseMetrics(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if p != 3 || a != 1 || bi != 0 || bo != 0 || sess != 0 {
		t.Fatalf("got parked=%d active=%d in=%d out=%d sess=%d", p, a, bi, bo, sess)
	}
}

func TestParseMetricsParkedOnly(t *testing.T) {
	body := "dotvpn_relay_parked 0\n"
	p, a, _, _, _, err := parseMetrics(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if p != 0 || a != 0 {
		t.Fatalf("got parked=%d active=%d", p, a)
	}
}

func TestParseMetricsMissingParked(t *testing.T) {
	_, _, _, _, _, err := parseMetrics(strings.NewReader("dotvpn_relay_active 2\n"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSplitHostPortDefault(t *testing.T) {
	h, p, err := splitHostPortDefault(":443", "443")
	if err != nil || h != "" || p != "443" {
		t.Fatalf("got %q %q %v", h, p, err)
	}
	h, p, err = splitHostPortDefault("1.2.3.4:8443", "443")
	if err != nil || h != "1.2.3.4" || p != "8443" {
		t.Fatalf("got %q %q %v", h, p, err)
	}
}
