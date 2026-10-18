// Package testfakes holds the stand-ins the core, daemon and TUI tests drive an app with: a
// store in memory, transcripts they can fill, a worktree factory and launchers that record
// what they are asked to do, plus a daemon serving one app in-process over net.Pipe.
package testfakes

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"hearth/internal/core"
)

// Store keeps sessions in memory; Saved is what the app wrote, by session id.
type Store struct {
	Loaded []*core.Session
	Saved  map[string]core.Session
}

func (s *Store) Load() ([]*core.Session, error) { return s.Loaded, nil }
func (s *Store) Save(ss *core.Session) error    { s.Saved[ss.SessionID] = *ss; return nil }

func (s *Store) Delete(id string) error {
	if _, ok := s.Saved[id]; !ok {
		return os.ErrNotExist
	}
	delete(s.Saved, id)
	return nil
}

// Transcripts is the saved transcript of each CLI session id; a missing one loads empty.
type Transcripts map[string][]core.Entry

func (t Transcripts) Load(cwd, cliSessionID string) ([]core.Entry, error) {
	return t[cliSessionID], nil
}

// Worktrees answers IsRepo as told and cuts a worktree at <origin>/.claude/worktrees/wt.
type Worktrees struct {
	Repo      bool
	RemoveErr error
}

func (w *Worktrees) IsRepo(string) bool { return w.Repo }

func (w *Worktrees) Create(originCwd, prompt string) (core.Worktree, error) {
	p := filepath.Join(originCwd, ".claude", "worktrees", "wt")
	return core.Worktree{Cwd: p, Path: p, Name: "wt", Branch: "claude/wt", SourceBranch: "main"}, nil
}

func (w *Worktrees) Remove(originCwd, path string) error { return w.RemoveErr }

// Launcher starts recording agents and lists Offers as its models.
type Launcher struct {
	Agents []*Agent
	Offers []core.Model
	Lists  int // Models calls
}

func (l *Launcher) Models(cwd string) ([]core.Model, error) { l.Lists++; return l.Offers, nil }

func (l *Launcher) Start(s *core.Session, out chan<- core.AgentEvent) (core.Agent, error) {
	ag := &Agent{out: out, Asked: make(chan struct{}, 16)}
	l.Agents = append(l.Agents, ag)
	return ag, nil
}

// AllowCall is one permission the app answered with allow.
type AllowCall struct {
	RequestID   string
	Input       map[string]any
	Permissions []any
}

// Agent records what core asked of it; request ids are "req-<n>" in call order.
type Agent struct {
	Sent     []string
	Requests []string
	Asked    chan struct{} // one signal per request, for a test to reply once the app is waiting
	Allowed  []AllowCall
	out      chan<- core.AgentEvent
}

// Tell reports an event s's agent would, as the real one's goroutine does.
func (a *Agent) Tell(s *core.Session, ev core.Event) {
	a.out <- core.AgentEvent{SessionID: s.SessionID, Event: ev}
}

func (a *Agent) req(r string) string {
	a.Requests = append(a.Requests, r)
	select {
	case a.Asked <- struct{}{}:
	default: // nobody is waiting on it
	}
	return fmt.Sprintf("req-%d", len(a.Requests))
}

func (a *Agent) SendUser(text, uuid string) error        { a.Sent = append(a.Sent, text); return nil }
func (a *Agent) Interrupt() string                       { return a.req("interrupt") }
func (a *Agent) CancelQueued(uuid string) string         { return a.req("cancel " + uuid) }
func (a *Agent) SetPermissionMode(mode string) string    { return a.req("mode " + mode) }
func (a *Agent) SetModel(model string) string            { return a.req("model " + model) }
func (a *Agent) SetEffort(effort string) string          { return a.req("effort " + effort) }
func (a *Agent) Rename(title string) string              { return a.req("rename " + title) }
func (a *Agent) GenerateTitle(description string) string { return a.req("title " + description) }

func (a *Agent) Rewind(id string, dryRun bool) string {
	return a.req(fmt.Sprint("rewind ", id, dryRun))
}

func (a *Agent) Allow(requestID string, input map[string]any, permissions []any) error {
	a.Allowed = append(a.Allowed, AllowCall{requestID, input, permissions})
	return nil
}

func (a *Agent) Deny(requestID, message string) error { return nil }
func (a *Agent) Close()                               {}

// Fakes is everything an app was built on.
type Fakes struct {
	Store       *Store
	Transcripts Transcripts
	Worktrees   *Worktrees
	Launcher    *Launcher // the default backend's
	PiLauncher  *Launcher // a backend without rewind or unqueue
}

// App builds an app over fresh fakes: claude, which can rewind and unqueue, and pi, which
// can do neither.
func App(t *testing.T, loaded ...*core.Session) (*core.App, *Fakes) {
	t.Helper()
	f := &Fakes{Store: &Store{Loaded: loaded, Saved: map[string]core.Session{}}, Transcripts: Transcripts{},
		Worktrees: &Worktrees{}, Launcher: &Launcher{}, PiLauncher: &Launcher{}}
	a, err := core.NewApp(f.Store, map[string]core.Backend{
		core.DefaultBackend: {Label: "Claude Code", Tag: "cl", Launcher: f.Launcher, Transcripts: f.Transcripts, Rewind: true, Unqueue: true},
		"pi":                {Label: "pi", Tag: "pi", Launcher: f.PiLauncher, Transcripts: f.Transcripts},
	}, f.Worktrees)
	if err != nil {
		t.Fatal(err)
	}
	return a, f
}

// WithAgent gives s an agent with no turn running, as a session that has talked before.
func WithAgent(a *core.App, s *core.Session) *Agent {
	ag, err := a.EnsureAgent(s)
	if err != nil {
		panic(err) // the fake launcher never fails
	}
	return ag.(*Agent)
}
