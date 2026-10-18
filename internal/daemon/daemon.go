package daemon

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"hearth/internal/core"
)

const (
	outQueue = 256 // queued messages before the daemon drops a client that cannot keep up
	lockWait = 100 // lock retries, 20ms apart: long enough for a daemon to exit
)

// Daemon drives one app for every client attached to it.
type Daemon struct {
	app       *core.App
	IdleAfter time.Duration // how long it may sit idle before exiting

	started time.Time
	mu      sync.Mutex
	peers   map[*peer]bool
	notice  string // the daemon notice clients were last told

	waits []*wait // requests the loop answers once an agent does

	joins  chan *peer
	leaves chan *peer
	asks   chan ask
	done   chan struct{} // closed when Serve returns
}

// ask is one client request waiting for the loop, with the peer that sent it.
type ask struct {
	from *peer
	request
}

// wait is a request whose answer comes from an agent, so the loop sends its reply later.
type wait struct {
	p       *peer
	id      int
	session string
	asked   string // the agent request it waits on
}

// errLater is what apply returns for a request it has parked as a wait.
var errLater = errors.New("answered later")

// peer is one attached connection: a reader goroutine feeding asks and a writer
// goroutine draining a bounded queue.
type peer struct {
	conn    net.Conn
	dec     *json.Decoder
	version string
	sent    map[string]int // transcript entries this client has, by session id
	out     chan []byte
	done    chan struct{} // closed when the writer has stopped
	dropped bool          // the loop dropped it for falling behind: its queue is closed
}

// New returns a daemon that owns app.
func New(app *core.App) *Daemon {
	return &Daemon{
		app:       app,
		IdleAfter: 30 * time.Second,
		started:   time.Now(),
		peers:     map[*peer]bool{},
		joins:     make(chan *peer),
		leaves:    make(chan *peer),
		asks:      make(chan ask),
		done:      make(chan struct{}),
	}
}

// Serve drives app for the clients that reach it on ln, until a client stops it, it goes
// idle, or ln fails.
func (d *Daemon) Serve(ln net.Listener) error {
	defer ln.Close()
	defer d.hangUp()
	defer close(d.done)
	defer d.app.CloseAll()
	go d.accept(ln)
	idle := time.NewTimer(d.IdleAfter)
	defer idle.Stop()
	for {
		idle.Stop()
		if d.idle() {
			idle.Reset(d.IdleAfter)
		}
		select {
		case m := <-d.app.Events():
			d.event(m)
		case p := <-d.joins:
			if d.join(p) {
				return nil
			}
		case p := <-d.leaves:
			d.leave(p)
		case a := <-d.asks:
			if d.serve(a) {
				return nil
			}
		case <-idle.C:
			if !d.idle() {
				continue // it fired while a message was in flight
			}
			return nil
		}
	}
}

// accept takes connections and hands each to the loop once it has said hello.
func (d *Daemon) accept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go d.hello(conn)
	}
}

// hello admits one connection: only our own user may attach, and it must send its version
// first. The loop answers it.
func (d *Daemon) hello(conn net.Conn) {
	if err := peerOK(conn); err != nil {
		conn.Close()
		return
	}
	p := &peer{conn: conn, dec: json.NewDecoder(conn), sent: map[string]int{},
		out: make(chan []byte, outQueue), done: make(chan struct{})}
	var m msg
	if err := p.dec.Decode(&m); err != nil || m.Hello == nil {
		conn.Close()
		return
	}
	p.version = m.Hello.Version
	select {
	case d.joins <- p:
	case <-d.done:
		conn.Close()
	}
}

// join answers a connection's hello: a client of another version is told whether the daemon
// is busy and turned away, a client of ours gets the snapshot and its goroutines. It reports
// whether the daemon should exit: that is an idle daemon meeting a client it cannot serve,
// which has nothing left to do but make way for that client's own build.
func (d *Daemon) join(p *peer) bool {
	if p.version != Version {
		busy := len(d.running()) > 0 // attached clients alone don't make it worth keeping
		b := marshal(msg{Hello: &hello{Version: Version, Busy: busy}})
		p.conn.SetWriteDeadline(time.Now().Add(time.Second))
		p.conn.Write(b)
		p.conn.Close()
		return !busy
	}
	d.mu.Lock()
	d.peers[p] = true
	d.mu.Unlock()
	snap := d.snapshot(p)
	d.queue(p, marshal(msg{Snapshot: &snap}))
	go d.read(p)
	go p.write()
	return false
}

// read feeds the loop every request p's client sends, until the connection ends.
func (d *Daemon) read(p *peer) {
	for {
		var m msg
		err := p.dec.Decode(&m)
		if err == nil && m.Request == nil {
			err = errors.New("not a request")
		}
		if err != nil {
			select {
			case d.leaves <- p:
			case <-d.done:
			}
			return
		}
		select {
		case d.asks <- ask{from: p, request: *m.Request}:
		case <-d.done:
			return
		}
	}
}

// write drains p's queue to its socket until the queue is closed or the connection
// breaks. Closing the connection is left to whoever closed the queue.
func (p *peer) write() {
	defer close(p.done)
	for b := range p.out {
		if _, err := p.conn.Write(b); err != nil {
			p.conn.Close()
			return
		}
	}
}

// leave forgets a client that disconnected, after its writer has put out what it holds.
func (d *Daemon) leave(p *peer) {
	d.mu.Lock()
	_, ok := d.peers[p]
	delete(d.peers, p)
	d.mu.Unlock()
	if !ok {
		return
	}
	d.waits = slices.DeleteFunc(d.waits, func(w *wait) bool { return w.p == p })
	p.conn.SetWriteDeadline(time.Now().Add(time.Second))
	close(p.out)
	<-p.done
	p.conn.Close()
}

// hangUp closes every attached connection, letting writers flush what they still hold.
func (d *Daemon) hangUp() {
	d.mu.Lock()
	peers := slices.Collect(maps.Keys(d.peers))
	clear(d.peers)
	d.mu.Unlock()
	for _, p := range peers {
		p.conn.SetWriteDeadline(time.Now().Add(time.Second))
		close(p.out)
	}
	for _, p := range peers {
		<-p.done
		p.conn.Close()
	}
}

// send encodes m once and queues it for every attached client, dropping the ones that
// cannot keep up. Encoding here, on the loop, keeps clients from reading state the app
// goes on to change.
func (d *Daemon) send(m msg) {
	b := marshal(m)
	d.mu.Lock()
	peers := slices.Collect(maps.Keys(d.peers))
	d.mu.Unlock()
	for _, p := range peers {
		d.queue(p, b)
	}
}

// queue queues b for one client, dropping it when its queue is full. Only the loop queues
// and closes, so a client dropped here is skipped for the rest of the pass.
func (d *Daemon) queue(p *peer, b []byte) {
	if p.dropped {
		return
	}
	select {
	case p.out <- b:
	default:
		p.dropped = true
		d.mu.Lock()
		delete(d.peers, p)
		d.mu.Unlock()
		close(p.out)
		go p.drop()
	}
}

// drop disconnects a client that could not keep up, telling it why if it still reads.
func (p *peer) drop() {
	<-p.done
	p.conn.SetWriteDeadline(time.Now().Add(time.Second))
	p.conn.Write(marshal(msg{Bye: "too slow"}))
	p.conn.Close()
}

// marshal encodes one message; our own types always allow it.
func marshal(m msg) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

// snapshot is everything a new client needs; it also notes how much each client has, so
// the changes that follow only carry what is new.
func (d *Daemon) snapshot(p *peer) snapshot {
	snap := snapshot{Live: map[string]*core.Live{}, Backends: d.app.Caps(),
		Models: d.app.ListedModels()}
	for _, s := range d.app.Sessions() {
		l := d.app.L(s.SessionID)
		live := *l
		if !live.Loaded {
			live.Entries = nil
		}
		snap.Sessions = append(snap.Sessions, s)
		snap.Live[s.SessionID] = &live
		p.sent[s.SessionID] = len(live.Entries)
	}
	d.mu.Lock()
	snap.Notice = d.notice
	d.mu.Unlock()
	return snap
}

// event applies one agent report and tells clients what it changed.
func (d *Daemon) event(m core.AgentEvent) {
	before := len(d.app.Sessions())
	d.app.Handle(m)
	d.failWaits(m)
	if e, ok := m.Event.(core.ModelsListed); ok {
		d.send(msg{Change: &change{Models: &models{Backend: e.Backend, Items: e.Models}}})
	}
	// start_agent puts the sessions it starts first, and an agent's report lands on its parent
	ss := d.app.Sessions()
	for _, s := range ss[:max(0, len(ss)-before)] {
		d.changed(s.SessionID)
	}
	if s := d.app.Find(m.SessionID); s != nil {
		d.changed(s.SessionID)
		if s.SpawnedFrom != nil {
			d.changed(s.SpawnedFrom.SessionID)
		}
	}
	d.daemonNotice()
}

// changed tells clients what a session now looks like.
func (d *Daemon) changed(id string) {
	s := d.app.Find(id)
	if s == nil {
		return // gone; its deletion went out as a change of its own
	}
	d.mu.Lock()
	peers := slices.Collect(maps.Keys(d.peers))
	d.mu.Unlock()
	for _, p := range peers {
		d.queue(p, marshal(msg{Change: d.change(p, s)}))
	}
}

// forget drops a deleted session from every client's bookkeeping.
func (d *Daemon) forget(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for p := range d.peers {
		delete(p.sent, id)
	}
}

// change brings p's copy of s up to date. The splice starts at the entry before what the
// client is missing, so an entry still streaming to it grows instead of being kept stale.
func (d *Daemon) change(p *peer, s *core.Session) *change {
	l := d.app.L(s.SessionID)
	from := max(0, min(p.sent[s.SessionID], len(l.Entries))-1)
	p.sent[s.SessionID] = len(l.Entries)
	live := *l
	live.Entries = nil
	return &change{Session: s, Live: &live, Entries: &splice{From: from, Items: slices.Clone(l.Entries[from:])}}
}

// daemonNotice tells every client when the daemon's own notice changes. Notices the TUI
// raises for its own input stay in that client.
func (d *Daemon) daemonNotice() {
	n := d.app.Notice
	if n == "" {
		return
	}
	d.app.Notice = "" // a client clears its own copy, so core's next notice must count as new
	d.mu.Lock()
	d.notice = n
	d.mu.Unlock()
	d.send(msg{Change: &change{Notice: &n}})
}

// serve runs one client request, sends the changes it caused before the reply, and
// reports whether the client asked the daemon to stop.
func (d *Daemon) serve(a ask) bool {
	res, err := d.apply(a.from, a.ID, a.Method, a.Args)
	d.daemonNotice()
	if err != errLater {
		d.answer(a.from, a.ID, res, err)
	}
	return a.Method == "stop"
}

// apply runs one request against the app, tells clients what it changed, and returns the
// request's small answer, or errLater when an agent's reply is still to give it.
func (d *Daemon) apply(p *peer, id int, method string, a args) (any, error) {
	// a client may name a session another one has deleted since; sessionless methods name none
	var sess *core.Session
	if a.Session != "" {
		if sess = d.app.Find(a.Session); sess == nil {
			return nil, errors.New(noSession)
		}
	}
	s := func() *core.Session { return sess }
	switch method {
	case "send":
		err := d.app.Send(s(), a.Text)
		d.changed(a.Session)
		return nil, err
	case "interrupt":
		d.app.Interrupt(s())
		return nil, nil
	case "interruptAndSend":
		err := d.app.InterruptAndSend(s(), a.Text)
		d.changed(a.Session)
		return nil, err
	case "unqueue":
		w := &wait{p: p, id: id, session: a.Session}
		return d.park(w, d.app.Unqueue(s(), func(t string) { d.finish(w, t, nil) }))
	case "ensureLoaded":
		d.app.EnsureLoaded(s())
		d.changed(a.Session)
		return nil, nil
	case "previewRewind":
		w := &wait{p: p, id: id, session: a.Session}
		return d.park(w, d.app.PreviewRewind(s(), a.Turn, func(r core.RewindPreview) { d.finish(w, r, nil) }))
	case "rewind":
		return nil, d.app.Rewind(s(), a.Turn)
	case "newSession":
		ns := d.app.NewSession(a.Dir, a.Worktree)
		d.changed(ns.SessionID)
		return ns.SessionID, nil
	case "acceptChip":
		parent := s()
		child, err := d.app.AcceptChip(parent, a.Index)
		// the chip left the parent either way, and a child that started is a session everywhere
		d.changed(parent.SessionID)
		if child != nil {
			d.changed(child.SessionID)
		}
		if err != nil {
			return nil, err
		}
		return child.SessionID, nil
	case "delete":
		ss := s()
		if err := d.app.Delete(ss); err != nil {
			return nil, err
		}
		d.send(msg{Change: &change{Deleted: ss.SessionID}})
		d.forget(ss.SessionID)
		for _, w := range slices.Clone(d.waits) {
			if w.session == ss.SessionID {
				d.finish(w, nil, errors.New("session deleted"))
			}
		}
		return nil, nil
	case "answerPerm":
		err := d.app.AnswerPerm(s(), a.Allow, a.Always)
		d.changed(a.Session)
		return nil, err
	case "answerQuestion":
		err := d.app.AnswerQuestion(s(), a.Text)
		d.changed(a.Session)
		return nil, err
	case "setBackend":
		err := d.app.SetBackend(s(), a.Backend)
		d.changed(a.Session)
		return nil, err
	case "models":
		d.app.Models(s()) // starts the listing; the list arrives as a change
		return nil, nil
	case "isRepo":
		return d.app.IsRepo(a.Dir), nil
	case "status":
		return d.status(p), nil
	case "stop":
		return nil, nil
	}
	// the rest only change the session record or its live data
	switch method {
	case "dismissChip":
		d.app.DismissChip(s(), a.Index)
	case "rename":
		d.app.Rename(s(), a.Title)
	case "toggleStar":
		d.app.ToggleStar(s())
	case "toggleArchive":
		d.app.ToggleArchive(s())
	case "setModel":
		d.app.SetModel(s(), a.Model)
	case "setEffort":
		d.app.SetEffort(s(), a.Effort)
	case "setMode":
		d.app.SetMode(s(), a.Mode)
	default:
		return nil, fmt.Errorf("unknown method %q", method)
	}
	d.changed(a.Session)
	return nil, nil
}

// answer sends a request's reply, with result as its JSON body.
func (d *Daemon) answer(p *peer, id int, value any, err error) {
	r := reply{ID: id}
	switch {
	case err != nil:
		r.Err = err.Error()
	case value != nil:
		b, mErr := json.Marshal(value)
		if mErr != nil {
			panic(mErr)
		}
		r.Result = b
	}
	d.queue(p, marshal(msg{Reply: &r}))
}

// park holds w until the agent answers the request err says was made, or answers the request
// with err when it wasn't.
func (d *Daemon) park(w *wait, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	w.asked = d.app.Asked(d.app.Find(w.session))
	d.waits = append(d.waits, w)
	return nil, errLater
}

// finish answers w, after the changes its request caused. Once is enough: an answer for a
// wait that failed first, or whose client left, goes nowhere.
func (d *Daemon) finish(w *wait, value any, err error) {
	i := slices.Index(d.waits, w)
	if i < 0 {
		return
	}
	d.waits = slices.Delete(d.waits, i, i+1)
	d.changed(w.session)
	d.answer(w.p, w.id, value, err)
}

// failWaits answers with an error the waits that m means no agent reply will come for: an
// error reply drops its callback without running it, and so does an agent that exited.
func (d *Daemon) failWaits(m core.AgentEvent) {
	for _, w := range slices.Clone(d.waits) {
		if w.session != m.SessionID {
			continue
		}
		switch e := m.Event.(type) {
		case core.Exited:
			d.finish(w, nil, errors.New("agent exited"))
		case core.Reply:
			if e.RequestID == w.asked {
				d.finish(w, nil, errors.New(d.app.Notice))
			}
		}
	}
}

// idle reports whether nothing would be lost if the daemon exited.
func (d *Daemon) idle() bool {
	return d.count() == 0 && len(d.running()) == 0
}

// count is how many clients are attached.
func (d *Daemon) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.peers)
}

// running lists the sessions a stop would cut short: a running turn, a pending
// permission or a queued message.
func (d *Daemon) running() []string {
	var out []string
	for _, s := range d.app.Sessions() {
		if l := d.app.L(s.SessionID); l.Running || len(l.Perms) > 0 || len(l.Queued) > 0 {
			out = append(out, cmp.Or(s.Title, s.SessionID))
		}
	}
	return out
}

// status reports what the daemon is doing. The client asking isn't counted: the tool
// asking wants to know how many hearths are attached.
func (d *Daemon) status(p *peer) Status {
	return Status{PID: os.Getpid(), Started: d.started, Version: Version,
		Clients: max(0, d.count()-1), Running: d.running()}
}

// Run runs the daemon for home in the foreground until it is stopped or goes idle.
func Run(app *core.App, home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	lock, err := lockDaemon(LockPath(home))
	if err != nil {
		return fmt.Errorf("a hearth daemon is already running in %s", home)
	}
	defer lock.Close()
	sock := SocketPath(home)
	// we hold the lock, so a socket file here belongs to a daemon that is gone
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer os.Remove(sock)
	return New(app).Serve(ln)
}

// lockDaemon takes an exclusive lock on path for the daemon's lifetime, so only one runs per
// HEARTH_HOME. The kernel drops it when the process dies, however it dies. It waits a moment
// for a daemon that is on its way out, so the one starting in its place still gets the lock.
func lockDaemon(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for try := 0; ; try++ {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || try == lockWait {
			f.Close()
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
