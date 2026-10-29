package relay

import (
	"bytes"
	"io"
	"net"
	"sync"
)

// parked is an authenticated, idle pool connection from a node. A pump
// goroutine owns the carrier's Read from the start, so a node that closes the
// connection is noticed at once, and the same goroutine feeds the splice once
// a client takes the connection. The node sends nothing while idle, so the
// first bytes to arrive are always the client's relayed handshake.
type parked struct {
	c    net.Conn
	data chan []byte
	dead chan struct{}
	done chan struct{}
	once sync.Once
	err  error
	left []byte
}

func newParked(c net.Conn) *parked {
	p := &parked{c: c, data: make(chan []byte), dead: make(chan struct{}), done: make(chan struct{})}
	go p.pump()
	return p
}

func (p *parked) pump() {
	defer close(p.dead)
	defer close(p.data)
	buf := make([]byte, 32<<10)
	for {
		n, err := p.c.Read(buf)
		if n > 0 {
			select {
			case p.data <- bytes.Clone(buf[:n]):
			case <-p.done:
				return
			}
		}
		if err != nil {
			p.err = err
			return
		}
	}
}

func (p *parked) Read(b []byte) (int, error) {
	if len(p.left) == 0 {
		d, ok := <-p.data
		if !ok {
			if p.err == nil {
				return 0, io.EOF
			}
			return 0, p.err
		}
		p.left = d
	}
	n := copy(b, p.left)
	p.left = p.left[n:]
	return n, nil
}

func (p *parked) Write(b []byte) (int, error) { return p.c.Write(b) }

func (p *parked) Close() error {
	p.once.Do(func() { close(p.done) })
	return p.c.Close()
}

func (p *parked) alive() bool {
	select {
	case <-p.dead:
		return false
	default:
		return true
	}
}

type entry struct {
	key   []byte // client knock key for this node
	conns []*parked
}

// pool holds idle connections per node id. Bounded per node and in total so a
// compromised or buggy node cannot exhaust the relay.
type pool struct {
	maxPer, maxTotal int

	mu    sync.Mutex
	nodes map[string]*entry
	total int
}

func newPool(maxPer, maxTotal int) *pool {
	return &pool{maxPer: maxPer, maxTotal: maxTotal, nodes: map[string]*entry{}}
}

// add parks c for node id. It reports false (caller closes c) at the limits.
func (p *pool) add(id string, key []byte, c *parked) bool {
	p.mu.Lock()
	e := p.nodes[id]
	if e == nil {
		e = &entry{key: key}
		p.nodes[id] = e
	}
	if len(e.conns) >= p.maxPer || p.total >= p.maxTotal {
		p.mu.Unlock()
		return false
	}
	e.conns = append(e.conns, c)
	p.total++
	p.mu.Unlock()
	go func() {
		<-c.dead
		p.remove(id, c)
	}()
	return true
}

func (p *pool) remove(id string, c *parked) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.nodes[id]
	if e == nil {
		return
	}
	for i, x := range e.conns {
		if x == c {
			e.conns = append(e.conns[:i], e.conns[i+1:]...)
			p.total--
			break
		}
	}
	if len(e.conns) == 0 {
		delete(p.nodes, id)
	}
}

// take removes and returns the newest live connection of node id.
func (p *pool) take(id string) *parked {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.nodes[id]
	for e != nil && len(e.conns) > 0 {
		c := e.conns[len(e.conns)-1]
		e.conns = e.conns[:len(e.conns)-1]
		p.total--
		if len(e.conns) == 0 {
			delete(p.nodes, id)
			e = nil
		}
		if c.alive() {
			return c
		}
		_ = c.Close()
	}
	return nil
}

// keys are the client knock keys of nodes that currently have a connection.
func (p *pool) keys() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, 0, len(p.nodes))
	for _, e := range p.nodes {
		out = append(out, e.key)
	}
	return out
}

func (p *pool) nodeFor(key []byte) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, e := range p.nodes {
		if bytes.Equal(e.key, key) {
			return id, true
		}
	}
	return "", false
}

func (p *pool) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
}
