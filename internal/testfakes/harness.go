package testfakes

import (
	"net"
	"sync"
	"testing"
	"time"

	"hearth/internal/core"
	"hearth/internal/daemon"
)

// Harness is one daemon serving a fresh app in-process, with clients dialing into it: each
// Dial is another attached instance.
type Harness struct {
	*Fakes
	App      *core.App
	listener *pipeListener
	served   chan error
	clients  []*daemon.Client
}

// NewHarness builds an app over fresh fakes and serves it. The daemon exits once it has
// been idle for idle, so a test that wants it up for the whole run passes a long one.
func NewHarness(t *testing.T, idle time.Duration, loaded ...*core.Session) *Harness {
	t.Helper()
	app, f := App(t, loaded...)
	h := &Harness{Fakes: f, App: app, listener: &pipeListener{conns: make(chan net.Conn)}, served: make(chan error, 1)}
	d := daemon.New(app)
	d.IdleAfter = idle
	go func() { h.served <- d.Serve(h.listener) }()
	t.Cleanup(h.Close)
	return h
}

// Dial attaches a client and returns it once the daemon has sent its snapshot.
func (h *Harness) Dial(t *testing.T) *daemon.Client {
	t.Helper()
	c, err := daemon.NewClient(h.pipe())
	if err != nil {
		t.Fatal(err)
	}
	h.clients = append(h.clients, c)
	t.Cleanup(c.Close)
	return c
}

// DialVersion attaches a client that speaks another protocol version, as a build of
// another vintage would; it fails with a *daemon.VersionError when the daemon runs one.
func (h *Harness) DialVersion(version string) (*daemon.Client, error) {
	c, err := daemon.NewClientVersion(h.pipe(), version)
	if err != nil {
		return nil, err
	}
	h.clients = append(h.clients, c)
	return c, nil
}

// Conn hands a test one end of a pipe into the daemon with no client on it: a peer that
// speaks the protocol itself and can stop reading.
func (h *Harness) Conn(t *testing.T) net.Conn {
	t.Helper()
	c := h.pipe()
	t.Cleanup(func() { c.Close() })
	return c
}

// pipe hands the daemon one end of a pipe and the client the other.
func (h *Harness) pipe() net.Conn {
	client, server := net.Pipe()
	h.listener.conns <- server
	return client
}

// Served carries Serve's return once the daemon exits.
func (h *Harness) Served() <-chan error { return h.served }

// Close detaches the clients and stops the accepting, as quitting every instance would.
func (h *Harness) Close() {
	for _, c := range h.clients {
		c.Close()
	}
	h.listener.Close()
}

// pipeListener hands the daemon the connections a test made, one per Accept.
type pipeListener struct {
	conns chan net.Conn
	once  sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	c, ok := <-l.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.conns) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }
