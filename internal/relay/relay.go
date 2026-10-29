// Package relay is the stealth hop in a restricted country. It only listens:
// the core node dials out and parks authenticated connections here, clients
// are spliced onto them and run the session handshake with the node through
// the relay, which therefore forwards ciphertext only. The relay stores no
// secret beyond its own identity key, knows no core address, and logs no
// remote address, domain or key.
//
// Everything that is not an authenticated dotvpn peer is handed to a real
// server (see decoy.go), so probes see an ordinary website.
package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/payam-foundation/dotvpn-wire/auth"
	"github.com/payam-foundation/dotvpn-wire/dnscarrier"
	"github.com/payam-foundation/dotvpn-wire/frame"
	"github.com/payam-foundation/dotvpn-wire/handshake"
	"github.com/payam-foundation/dotvpn-wire/mimic"
	"github.com/payam-foundation/dotvpn-wire/transport"
	"github.com/payam-foundation/dotvpn-wire/transport/reverse"
)

// Options configure a Server.
type Options struct {
	// Identity is the relay's Ed25519 key. Nodes pin its public half.
	Identity ed25519.PrivateKey
	// Verifier checks pool tokens (auth.AudPool) against control's keys.
	Verifier *auth.Verifier
	// Fallback answers connections that are not authenticated peers.
	Fallback func(net.Conn)
	// MaxConns bounds concurrent connections. Default 4096.
	MaxConns int
	// MaxPoolPerNode and MaxPool bound parked connections. Defaults 16, 1024.
	MaxPoolPerNode, MaxPool int
}

// Server is a relay.
type Server struct {
	o       Options
	pub     ed25519.PublicKey
	poolKey []byte
	pool    *pool
	mimic   *mimic.Server
	sem     chan struct{}
	frame   frame.Opts
}

// New validates o and returns a Server.
func New(o Options) (*Server, error) {
	switch {
	case len(o.Identity) != ed25519.PrivateKeySize:
		return nil, errors.New("relay: identity key required")
	case o.Verifier == nil:
		return nil, errors.New("relay: verifier required")
	case o.Fallback == nil:
		return nil, errors.New("relay: fallback required (a relay must answer probes like a real server)")
	}
	if o.MaxConns == 0 {
		o.MaxConns = 4096
	}
	if o.MaxPoolPerNode == 0 {
		o.MaxPoolPerNode = 16
	}
	if o.MaxPool == 0 {
		o.MaxPool = 1024
	}
	s := &Server{
		o:     o,
		pub:   o.Identity.Public().(ed25519.PublicKey),
		pool:  newPool(o.MaxPoolPerNode, o.MaxPool),
		sem:   make(chan struct{}, o.MaxConns),
		frame: frame.Opts{Idle: reverse.DefaultIdle},
	}
	s.poolKey = reverse.PoolKnockKey(s.pub)
	s.mimic = &mimic.Server{KeysFn: s.keys}
	return s, nil
}

// keys are the knock keys accepted right now: the pool key, plus the client
// key of every node with a parked connection. A client for a node without one
// is treated as any other visitor (it gets the decoy), never an error.
func (s *Server) keys() [][]byte {
	return append([][]byte{s.poolKey}, s.pool.keys()...)
}

// Parked is the number of idle node connections held.
func (s *Server) Parked() int { return s.pool.count() }

// ServeTCP accepts until ctx ends or ln fails.
func (s *Server) ServeTCP(ctx context.Context, ln net.Listener) error {
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		if !s.acquire() {
			_ = c.Close()
			continue
		}
		go func() {
			defer s.release()
			s.handleTCP(c)
		}()
	}
}

func (s *Server) acquire() bool {
	select {
	case s.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) release() { <-s.sem }

func (s *Server) handleTCP(c net.Conn) {
	key, conn, ok := s.mimic.ClassifyKey(c)
	if !ok {
		s.o.Fallback(conn)
		return
	}
	if bytes.Equal(key, s.poolKey) {
		s.acceptPool(conn)
		return
	}
	id, ok := s.pool.nodeFor(key)
	if !ok {
		_ = conn.Close()
		return
	}
	s.splice(conn, id)
}

// acceptPool authenticates a node and parks its connection. The node proves
// itself with a short-lived, control-signed pool token inside the encrypted
// channel, so nothing identifying it crosses the wire in the clear.
func (s *Server) acceptPool(c net.Conn) {
	outer, err := handshake.Respond(c, s.o.Identity, s.frame)
	if err != nil {
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(handshake.Timeout))
	id, err := reverse.ReadAuth(outer, s.o.Verifier)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		_ = outer.Close()
		return
	}
	p := newParked(outer)
	if !s.pool.add(id, reverse.ClientKnockKey(s.pub, id), p) {
		_ = p.Close()
	}
}

func (s *Server) splice(client net.Conn, nodeID string) {
	p := s.pool.take(nodeID)
	if p == nil {
		_ = client.Close()
		return
	}
	transport.Splice(client, p)
}

// ServeDNS serves the DNS-shaped UDP carrier for zone on pc. A client names
// the node first, then runs the session handshake through the relay.
func (s *Server) ServeDNS(ctx context.Context, pc net.PacketConn, zone string) error {
	l, err := dnscarrier.Listen(pc, zone)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = l.Close(); _ = pc.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !s.acquire() {
			_ = c.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.release()
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			id, err := reverse.ReadNodeID(c)
			_ = c.SetReadDeadline(time.Time{})
			if err != nil {
				_ = c.Close()
				return
			}
			s.splice(c, id)
		}()
	}
}
