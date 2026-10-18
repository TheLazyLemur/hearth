package daemon

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"

	"hearth/internal/core"
)

// Client is one attached instance: a copy of the daemon's state that every command
// updates on the daemon, plus the changes the daemon sends.
//
// The copy belongs to the goroutine that runs the TUI: read it there, call Drain there,
// and send commands from there, so it never changes under a draw. The socket is read by
// a goroutine of its own, which only queues replies and changes.
type Client struct {
	conn net.Conn
	enc  *json.Encoder

	ss       []*core.Session
	live     map[string]*core.Live
	backends map[string]core.Caps
	models   map[string][]core.Model
	notice   string

	encMu   sync.Mutex // one request on the wire at a time
	mu      sync.Mutex // the fields below are the reader's as well
	dec     *json.Decoder
	pending map[int]chan reply
	next    int
	changes []*change
	closed  chan struct{}
	err     error

	closeOnce sync.Once
}

// ErrGone is what a client's Err wraps when the daemon's side of the connection ended.
var ErrGone = errors.New("daemon exited")

// Dial connects to the daemon listening on socket.
func Dial(socket string) (*Client, error) { return DialVersion(socket, Version) }

// DialVersion connects to the daemon listening on socket, speaking another protocol
// version, as a build of another vintage would; it fails with a *VersionError.
func DialVersion(socket, version string) (*Client, error) {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil, err
	}
	return NewClientVersion(conn, version)
}

// NewClient attaches to a daemon over a connected socket.
func NewClient(conn net.Conn) (*Client, error) { return NewClientVersion(conn, Version) }

// NewClientVersion attaches to a daemon over a connected socket, speaking version: it says
// hello, takes the snapshot and starts reading. The caller owns conn and must Close it with
// the client.
func NewClientVersion(conn net.Conn, version string) (*Client, error) {
	c := &Client{conn: conn, enc: json.NewEncoder(conn), dec: json.NewDecoder(conn),
		live: map[string]*core.Live{}, backends: map[string]core.Caps{}, models: map[string][]core.Model{},
		pending: map[int]chan reply{}, closed: make(chan struct{})}
	if err := c.enc.Encode(msg{Hello: &hello{Version: version}}); err != nil {
		conn.Close()
		return nil, err
	}
	var m msg
	if err := c.dec.Decode(&m); err != nil {
		conn.Close()
		return nil, err
	}
	switch {
	case m.Hello != nil:
		conn.Close()
		return nil, &VersionError{Version: m.Hello.Version, Busy: m.Hello.Busy}
	case m.Snapshot == nil:
		conn.Close()
		return nil, errors.New("daemon sent no snapshot")
	}
	c.take(m.Snapshot)
	go c.read()
	return c, nil
}

// read queues everything the daemon sends until the connection ends.
func (c *Client) read() {
	defer close(c.closed)
	for {
		var m msg
		err := c.dec.Decode(&m)
		if err != nil {
			c.fail(err)
			return
		}
		c.mu.Lock()
		switch {
		case m.Reply != nil:
			if ch, ok := c.pending[m.Reply.ID]; ok {
				delete(c.pending, m.Reply.ID)
				ch <- *m.Reply
			}
			c.mu.Unlock()
		case m.Change != nil:
			c.changes = append(c.changes, m.Change)
			c.mu.Unlock()
		case m.Bye != "":
			c.err = errors.New("disconnected: " + m.Bye)
			c.mu.Unlock()
			return
		default:
			c.err = errors.New("daemon sent an unexpected message")
			c.mu.Unlock()
			return
		}
	}
}

// fail records why the connection ended.
func (c *Client) fail(err error) {
	c.mu.Lock()
	c.err = fmt.Errorf("%w: %v", ErrGone, err)
	c.mu.Unlock()
}

// take starts the copy from a snapshot, as the daemon sends one to each new client.
func (c *Client) take(s *snapshot) {
	c.ss, c.live, c.backends, c.notice = s.Sessions, s.Live, s.Backends, s.Notice
	clear(c.models)
	for b, ms := range s.Models {
		c.models[b] = ms
	}
}

// Drain applies the changes the daemon sent since the last call, reporting whether there
// were any. Run it once per pass of the TUI's event loop, before reading the copy.
func (c *Client) Drain() bool {
	c.mu.Lock()
	cs := c.changes
	c.changes = nil
	c.mu.Unlock()
	for _, ch := range cs {
		c.applyChange(ch)
	}
	return len(cs) > 0
}

// applyChange folds one change into the copy.
func (c *Client) applyChange(ch *change) {
	if ch.Deleted != "" {
		delete(c.live, ch.Deleted)
		c.ss = slices.DeleteFunc(c.ss, func(s *core.Session) bool { return s.SessionID == ch.Deleted })
		return
	}
	if ch.Models != nil {
		c.models[ch.Models.Backend] = ch.Models.Items
	}
	if ch.Notice != nil {
		c.notice = *ch.Notice
	}
	if ch.Session == nil {
		return
	}
	s, l := ch.Session, ch.Live
	if i := slices.IndexFunc(c.ss, func(o *core.Session) bool { return o.SessionID == s.SessionID }); i >= 0 {
		c.ss[i] = s
	} else {
		c.ss = append(c.ss, s)
	}
	if l == nil {
		return
	}
	var old []core.Entry
	if prev, ok := c.live[s.SessionID]; ok {
		old = prev.Entries
	}
	if e := ch.Entries; e != nil {
		l.Entries = append(slices.Clone(old[:e.From]), e.Items...)
	}
	c.live[s.SessionID] = l
}

// Closed is closed when the daemon connection ends.
func (c *Client) Closed() <-chan struct{} { return c.closed }

// Err is why the connection ended: "disconnected: too slow" for a client the daemon dropped
// for falling behind, ErrGone wrapped with the reason when the daemon went away. Read it
// once Closed fires.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cmp.Or(c.err, errors.New("daemon closed the connection"))
}

// Close detaches: the daemon keeps running, and everything it holds, for the next client.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.Drain()
		c.conn.Close()
	})
}

// result sends a request and decodes its answer into out.
func (c *Client) result(method string, a args, out any) error {
	b, err := c.call(method, a)
	if err != nil {
		return err
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

// call sends one request and waits for its reply, folding in the changes the daemon sends
// meanwhile. Requests are one at a time: the daemon runs them in order, in its own loop.
func (c *Client) call(method string, a args) (json.RawMessage, error) {
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan reply, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	c.encMu.Lock()
	err := c.enc.Encode(msg{Request: &request{ID: id, Method: method, Args: a}})
	c.encMu.Unlock()
	if err != nil {
		return c.forget(id, fmt.Errorf("%w: %v", ErrGone, err))
	}
	for {
		select {
		case r := <-ch:
			c.Drain()
			return r.Result, r.error()
		case <-c.closed:
			c.Drain()
			select { // a reply can land just before the connection ends
			case r := <-ch:
				return r.Result, r.error()
			default:
				return c.forget(id, c.Err())
			}
		}
	}
}

// forget drops a request that never got its reply.
func (c *Client) forget(id int, err error) (json.RawMessage, error) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
	return nil, err
}

func (r reply) error() error {
	if r.Err == "" {
		return nil
	}
	return errors.New(r.Err)
}

// Sessions lists every session in the copy, newest first.
func (c *Client) Sessions() []*core.Session { return c.ss }

// Visible lists sessions for the sidebar: starred first, then most recent.
func (c *Client) Visible(showArchived bool) []*core.Session { return core.Visible(c.ss, showArchived) }

// Find looks a session up by id.
func (c *Client) Find(id string) *core.Session {
	i := slices.IndexFunc(c.ss, func(s *core.Session) bool { return s.SessionID == id })
	if i < 0 {
		return nil
	}
	return c.ss[i]
}

// L lists the live data of a session; a session the daemon doesn't know is idle and empty.
func (c *Client) L(id string) *core.Live {
	if l, ok := c.live[id]; ok {
		return l
	}
	return &core.Live{}
}

// Status reports what a session is doing, for the sidebar.
func (c *Client) Status(s *core.Session) string { return c.L(s.SessionID).Status() }

// Backend is what s's backend offers.
func (c *Client) Backend(s *core.Session) core.Caps { return c.backends[s.Backend] }

// Efforts lists the effort levels s's model accepts, or nil when it isn't listed.
func (c *Client) Efforts(s *core.Session) []string {
	return core.Efforts(c.models[s.Backend], cmp.Or(s.Model, c.L(s.SessionID).Model))
}

// Notice is what the daemon last told every client.
func (c *Client) Notice() string { return c.notice }

// SetNotice shows text in this client only; it stays up until the next notice.
func (c *Client) SetNotice(text string) { c.notice = text }

// AnyRunning reports whether some session has a turn going.
func (c *Client) AnyRunning() bool {
	for _, s := range c.ss {
		if l := c.L(s.SessionID); l.Running {
			return true
		}
	}
	return false
}

// IsRepo reports whether dir is inside a git repository.
func (c *Client) IsRepo(dir string) bool {
	var repo bool
	c.check("isRepo", args{Dir: dir}, &repo)
	return repo
}

// Models lists the models s's backend offers, asking the daemon the first time and
// reporting loading until they arrive.
func (c *Client) Models(s *core.Session) (models []core.Model, loading bool) {
	if ms, ok := c.models[s.Backend]; ok {
		return ms, ms == nil
	}
	c.models[s.Backend] = nil
	c.check("models", args{Session: s.SessionID}, nil)
	return nil, true
}

// NewSession starts a session in dir, as its own worktree when worktree is set.
func (c *Client) NewSession(dir string, worktree bool) *core.Session {
	var id string
	c.check("newSession", args{Dir: dir, Worktree: worktree}, &id)
	return c.Find(id)
}

// Delete drops s and its saved file.
func (c *Client) Delete(s *core.Session) error {
	return c.result("delete", args{Session: s.SessionID}, nil)
}

// Rename sets s's title and keeps it as the agent's own.
func (c *Client) Rename(s *core.Session, title string) {
	c.check("rename", args{Session: s.SessionID, Title: title}, nil)
}

// ToggleStar stars or unstars s.
func (c *Client) ToggleStar(s *core.Session) { c.check("toggleStar", args{Session: s.SessionID}, nil) }

// ToggleArchive files s under the archive, or brings it back.
func (c *Client) ToggleArchive(s *core.Session) {
	c.check("toggleArchive", args{Session: s.SessionID}, nil)
}

// SetModel runs s on model from the next message on.
func (c *Client) SetModel(s *core.Session, model string) {
	c.check("setModel", args{Session: s.SessionID, Model: model}, nil)
}

// SetEffort runs s at effort from the next message on.
func (c *Client) SetEffort(s *core.Session, effort string) {
	c.check("setEffort", args{Session: s.SessionID, Effort: effort}, nil)
}

// SetMode runs s in permission mode from the next message on.
func (c *Client) SetMode(s *core.Session, mode string) {
	c.check("setMode", args{Session: s.SessionID, Mode: mode}, nil)
}

// SetBackend moves s to another harness from the next message on.
func (c *Client) SetBackend(s *core.Session, name string) error {
	return c.result("setBackend", args{Session: s.SessionID, Backend: name}, nil)
}

// EnsureLoaded loads s's transcript into the daemon and the copy.
func (c *Client) EnsureLoaded(s *core.Session) {
	c.check("ensureLoaded", args{Session: s.SessionID}, nil)
}

// Send hands text to s's agent, starting one if none runs.
func (c *Client) Send(s *core.Session, text string) error {
	return c.result("send", args{Session: s.SessionID, Text: text}, nil)
}

// Interrupt asks s's agent to stop the running turn.
func (c *Client) Interrupt(s *core.Session) { c.check("interrupt", args{Session: s.SessionID}, nil) }

// InterruptAndSend stops the running turn and sends text after it.
func (c *Client) InterruptAndSend(s *core.Session, text string) error {
	return c.result("interruptAndSend", args{Session: s.SessionID, Text: text}, nil)
}

// Unqueue takes back the newest queued message, handing its text to onCancel.
func (c *Client) Unqueue(s *core.Session, onCancel func(string)) error {
	var text string
	if err := c.result("unqueue", args{Session: s.SessionID}, &text); err != nil {
		return err
	}
	if onCancel != nil {
		onCancel(text)
	}
	return nil
}

// PreviewRewind asks the agent what rewinding to turn would undo, handing it to onPreview.
func (c *Client) PreviewRewind(s *core.Session, turn int, onPreview func(core.RewindPreview)) error {
	var pv core.RewindPreview
	if err := c.result("previewRewind", args{Session: s.SessionID, Turn: turn}, &pv); err != nil {
		return err
	}
	if onPreview != nil {
		onPreview(pv)
	}
	return nil
}

// Rewind rewinds s to the end of turn.
func (c *Client) Rewind(s *core.Session, turn int) error {
	return c.result("rewind", args{Session: s.SessionID, Turn: turn}, nil)
}

// AnswerPerm gives s's first pending permission request its answer.
func (c *Client) AnswerPerm(s *core.Session, allow, always bool) error {
	return c.result("answerPerm", args{Session: s.SessionID, Allow: allow, Always: always}, nil)
}

// AnswerQuestion answers s's pending question with text.
func (c *Client) AnswerQuestion(s *core.Session, text string) error {
	return c.result("answerQuestion", args{Session: s.SessionID, Text: text}, nil)
}

// AcceptChip starts the chip at index i as its own session.
func (c *Client) AcceptChip(s *core.Session, i int) (*core.Session, error) {
	var id string
	if err := c.result("acceptChip", args{Session: s.SessionID, Index: i}, &id); err != nil {
		return nil, err
	}
	return c.Find(id), nil
}

// DismissChip drops the chip at index i.
func (c *Client) DismissChip(s *core.Session, i int) {
	c.check("dismissChip", args{Session: s.SessionID, Index: i}, nil)
}

// Stop asks the daemon to exit.
func (c *Client) Stop() error { return c.result("stop", args{}, nil) }

// DaemonStatus reports what the daemon is doing.
func (c *Client) DaemonStatus() (Status, error) {
	var st Status
	err := c.result("status", args{}, &st)
	return st, err
}

// check runs a request whose only answer worth reading is its error, which cannot happen
// for the calls this client makes, bar two: the daemon going away, which the TUI's loop
// sees as Closed, and a session another client deleted first, which is told as a notice.
func (c *Client) check(method string, a args, out any) {
	err := c.result(method, a, out)
	select {
	case <-c.closed:
		return
	default:
	}
	switch {
	case err == nil || errors.Is(err, ErrGone):
	case err.Error() == noSession:
		c.notice = err.Error()
	default:
		panic(fmt.Sprintf("%s: %v", method, err))
	}
}
