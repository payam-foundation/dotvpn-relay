package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/payam-foundation/dotvpn-wire/auth"
	"github.com/payam-foundation/dotvpn-wire/transport"
	"github.com/payam-foundation/dotvpn-wire/transport/reverse"
)

// echoBackend is the node: any token "ok" is a user, Dial returns an echo.
type echoBackend struct{}

func (echoBackend) Authenticate(t string) (transport.User, error) {
	if t != "ok" {
		return transport.User{}, transport.ErrUnauthorized
	}
	return transport.User{ID: "u"}, nil
}

func (echoBackend) Dial(context.Context, transport.User, string) (net.Conn, error) {
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(b, b); _ = b.Close() }()
	return a, nil
}

type rig struct {
	addr     string
	relayPub ed25519.PublicKey
	nodePub  ed25519.PublicKey
	srv      *Server
	decoy    *httptest.Server
	ctx      context.Context
	cancel   context.CancelFunc
	signer   *auth.Signer
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctlPub, ctlPriv, _ := auth.GenerateKey()
	relayPub, relayPriv, _ := auth.GenerateKey()
	nodePub, nodePriv, _ := auth.GenerateKey()

	decoy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "nginx")
		_, _ = io.WriteString(w, "decoy "+r.URL.Path)
	}))
	t.Cleanup(decoy.Close)
	srv, err := New(Options{Identity: relayPriv, Verifier: auth.NewVerifier(ctlPub), Fallback: ForwardTo(decoy.Listener.Addr().String())})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTCP(ctx, ln) }()

	tok, _ := auth.NewSigner(ctlPriv).Mint(auth.Claims{Sub: "n1", Aud: auth.AudPool}, time.Hour)
	pool := &reverse.Pool{
		Relays: func() []reverse.Relay {
			return []reverse.Relay{{ID: "r1", Addr: ln.Addr().String(), SNI: "cdn.example.com", Key: relayPub}}
		},
		Identity: nodePriv, Token: func() string { return tok }, Size: 2,
	}
	go func() { _ = pool.Serve(ctx, echoBackend{}) }()
	r := &rig{addr: ln.Addr().String(), relayPub: relayPub, nodePub: nodePub, srv: srv, decoy: decoy, ctx: ctx, cancel: cancel, signer: auth.NewSigner(ctlPriv)}
	deadline := time.Now().Add(10 * time.Second)
	for srv.Parked() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("node never parked connections (parked=%d)", srv.Parked())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return r
}

func (r *rig) endpoint(node string) transport.Endpoint {
	return transport.Endpoint{Kind: reverse.KindReverse, Addr: r.addr, Host: "cdn.example.com", Params: map[string]string{
		reverse.ParamID:    auth.EncodeKey(r.nodePub),
		reverse.ParamKnock: base64.StdEncoding.EncodeToString(reverse.ClientKnockKey(r.relayPub, node)),
	}}
}

func TestClientReachesNodeThroughRelay(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer cancel()
	sess, err := reverse.Dialer{}.Dial(ctx, r.endpoint("n1"), func() string { return "ok" })
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	st, err := sess.OpenStream(ctx, "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, _ = st.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo: %q %v", buf, err)
	}
	// The pool refills: the taken connection is replaced.
	deadline := time.Now().Add(10 * time.Second)
	for r.srv.Parked() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if r.srv.Parked() < 2 {
		t.Fatal("pool not refilled")
	}
}

func TestWrongNodeKnockGetsDecoyNotReset(t *testing.T) {
	r := newRig(t)
	// A knock for a node that has no parked connection looks like any visitor.
	ep := r.endpoint("nobody")
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	if _, err := (reverse.Dialer{}).Dial(ctx, ep, func() string { return "ok" }); err == nil {
		t.Fatal("dial to an unknown node succeeded")
	}
}

// A scanner that speaks plain HTTP or TLS to the relay must get the decoy's
// real answer, byte for byte.
func TestProbesSeeDecoy(t *testing.T) {
	r := newRig(t)
	get := func(path string) string {
		c, err := net.Dial("tcp", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true, ServerName: "cdn.example.com"}) //nolint:gosec // probe against a test decoy
		_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
		cl := &http.Client{Transport: &http.Transport{DialTLSContext: func(context.Context, string, string) (net.Conn, error) { return tc, nil }}}
		resp, err := cl.Get("https://cdn.example.com" + path)
		if err != nil {
			t.Fatalf("tls probe: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.Header.Get("Server") + "|" + string(b)
	}
	if got := get("/x"); got != "nginx|decoy /x" {
		t.Fatalf("tls probe got %q", got)
	}

	// Plain HTTP on the TLS port: the decoy answers it as a real server does.
	direct := rawExchange(t, r.decoy.Listener.Addr().String(), "GET / HTTP/1.1\r\nHost: a\r\n\r\n")
	viaRelay := rawExchange(t, r.addr, "GET / HTTP/1.1\r\nHost: a\r\n\r\n")
	if direct == "" || direct != viaRelay {
		t.Fatalf("plain probe differs from decoy:\nrelay: %q\ndecoy: %q", viaRelay, direct)
	}
	// Garbage is answered by the decoy too (TLS alert or close, but its own).
	if g, d := rawExchange(t, r.addr, "\x00\x01garbage\r\n\r\n"), rawExchange(t, r.decoy.Listener.Addr().String(), "\x00\x01garbage\r\n\r\n"); g != d {
		t.Fatalf("garbage probe differs:\nrelay: %q\ndecoy: %q", g, d)
	}
}

func rawExchange(t *testing.T, addr, send string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, send)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, _ := io.ReadAll(c)
	return strings.TrimSpace(string(b))
}

func TestPoolTokenRequired(t *testing.T) {
	r := newRig(t)
	// A node holding a data-plane token cannot park connections.
	_, priv, _ := auth.GenerateKey()
	bad, _ := auth.NewSigner(priv).Mint(auth.Claims{Sub: "evil", Aud: auth.AudPool}, time.Hour)
	_, nodePriv, _ := auth.GenerateKey()
	pool := &reverse.Pool{
		Relays: func() []reverse.Relay {
			return []reverse.Relay{{ID: "r1", Addr: r.addr, SNI: "x.example.com", Key: r.relayPub}}
		},
		Identity: nodePriv, Token: func() string { return bad }, Size: 1,
	}
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	go func() { _ = pool.Serve(ctx, echoBackend{}) }()
	time.Sleep(1500 * time.Millisecond)
	if n := r.srv.Parked(); n != 2 {
		t.Fatalf("forged pool token parked a connection: parked=%d", n)
	}
}

func TestNewRequiresFallback(t *testing.T) {
	_, priv, _ := auth.GenerateKey()
	pub, _, _ := auth.GenerateKey()
	if _, err := New(Options{Identity: priv, Verifier: auth.NewVerifier(pub)}); err == nil {
		t.Fatal("relay without a decoy fallback must not start")
	}
}

// labelBackend is a node whose streams answer with its own name, so a test
// can tell which node a client reached.
type labelBackend struct{ name string }

func (labelBackend) Authenticate(string) (transport.User, error) { return transport.User{ID: "u"}, nil }

func (b labelBackend) Dial(context.Context, transport.User, string) (net.Conn, error) {
	a, c := net.Pipe()
	go func() { _, _ = io.WriteString(c, b.name); _ = c.Close() }()
	return a, nil
}

// One relay serves many nodes. A client reaches only the node its knock
// names, and a node without parked tunnels gets the decoy.
func TestRelayServesManyNodes(t *testing.T) {
	r := newRig(t) // also runs node n1 (echo backend)
	nodes := map[string]ed25519.PublicKey{}
	for _, id := range []string{"n2", "n3"} {
		pub, priv, _ := auth.GenerateKey()
		nodes[id] = pub
		tok, _ := r.signer.Mint(auth.Claims{Sub: id, Aud: auth.AudPool}, time.Hour)
		pool := &reverse.Pool{
			Relays: func() []reverse.Relay {
				return []reverse.Relay{{ID: "r1", Addr: r.addr, SNI: "cdn.example.com", Key: r.relayPub}}
			},
			Identity: priv, Token: func() string { return tok }, Size: 1,
		}
		go func() { _ = pool.Serve(r.ctx, labelBackend{name: id}) }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for r.srv.Parked() < 4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := r.srv.Parked(); got != 4 {
		t.Fatalf("parked=%d, want 4 (2 for n1, 1 each for n2, n3)", got)
	}
	for id, pub := range nodes {
		ep := r.endpoint(id)
		ep.Params[reverse.ParamID] = auth.EncodeKey(pub)
		ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
		sess, err := reverse.Dialer{}.Dial(ctx, ep, func() string { return "ok" })
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		st, err := sess.OpenStream(ctx, "x:1")
		if err != nil {
			t.Fatalf("%s stream: %v", id, err)
		}
		b := make([]byte, len(id))
		if _, err := io.ReadFull(st, b); err != nil || string(b) != id {
			t.Fatalf("client for %s reached %q (%v)", id, b, err)
		}
		_ = sess.Close()
		cancel()
	}
}
