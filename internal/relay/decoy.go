package relay

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/payam-foundation/dotvpn-wire/transport"
)

// A fallback takes over a connection that is not an authenticated dotvpn
// peer. It receives every byte already read, replayed in order, and must
// answer like a real server: never a silent close or a reset.

// ForwardTo relays unauthenticated connections byte for byte to addr, a real
// TLS server (a stock nginx on the same host, or any site). Content, timing
// and fingerprint are the real server's own.
func ForwardTo(addr string) func(net.Conn) {
	return func(c net.Conn) {
		d, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			_ = c.Close()
			return
		}
		transport.Splice(c, d)
	}
}

const page = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Welcome</title></head>
<body><h1>Welcome</h1><p>This site is under construction.</p></body></html>
`

// Builtin serves a genuine TLS web server with cert to unauthenticated
// connections. Prefer ForwardTo a stock server: Go's HTTP stack is itself a
// fingerprint. The returned stop closes the server.
func Builtin(cert tls.Certificate) (fallback func(net.Conn), stop func()) {
	l := &connListener{ch: make(chan net.Conn), done: make(chan struct{})}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if r.URL.Path != "/" && r.URL.Path != "/index.html" {
				w.WriteHeader(http.StatusNotFound)
			}
			_, _ = io.WriteString(w, page)
		}),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0), // errors name remote addresses
	}
	go func() { _ = srv.ServeTLS(l, "", "") }()
	return func(c net.Conn) {
			select {
			case l.ch <- c:
			case <-l.done:
				_ = c.Close()
			}
		}, func() {
			l.Close()
			_ = srv.Close()
		}
}

type connListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *connListener) Addr() net.Addr { return addr{} }

type addr struct{}

func (addr) Network() string { return "relay" }
func (addr) String() string  { return "relay" }
