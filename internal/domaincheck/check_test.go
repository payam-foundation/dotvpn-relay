package domaincheck

import (
	"net"
	"testing"
)

func TestIsCDN(t *testing.T) {
	if !IsCDN(net.ParseIP("104.16.1.1")) {
		t.Fatal("expected cloudflare 104.16")
	}
	if !IsCDN(net.ParseIP("172.67.1.1")) {
		t.Fatal("expected cloudflare 172.67")
	}
	if IsCDN(net.ParseIP("1.2.3.4")) {
		t.Fatal("plain IP should not be CDN")
	}
	if IsCDN(net.ParseIP("127.0.0.1")) {
		t.Fatal("loopback should not be CDN")
	}
}

func TestValidHostname(t *testing.T) {
	if !validHostname("relay.example.com") {
		t.Fatal("expected valid")
	}
	if validHostname("") || validHostname("bad..host") || validHostname("has space.com") {
		t.Fatal("expected invalid")
	}
}

func TestPublicDialAddr(t *testing.T) {
	if got := publicDialAddr("1.2.3.4:443"); got != "1.2.3.4:443" {
		t.Fatalf("got %q", got)
	}
	if got := publicDialAddr(":443"); got != "127.0.0.1:443" {
		t.Fatalf("got %q", got)
	}
}
