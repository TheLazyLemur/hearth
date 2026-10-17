package core

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type fakeStore struct{ saved map[string]Session }

func (f *fakeStore) Load() ([]*Session, error) { return nil, nil }
func (f *fakeStore) Save(s *Session) error     { f.saved[s.SessionID] = *s; return nil }
func (f *fakeStore) Delete(id string) error {
	if _, ok := f.saved[id]; !ok {
		return os.ErrNotExist
	}
	delete(f.saved, id)
	return nil
}

type fakeTranscripts struct{}

func (fakeTranscripts) Load(cwd, cliSessionID string) ([]Entry, error) { return nil, nil }

type fakeWorktrees struct {
	repo      bool
	removeErr error
}

func (f *fakeWorktrees) IsRepo(string) bool { return f.repo }
func (f *fakeWorktrees) Create(originCwd, prompt string) (Worktree, error) {
	p := filepath.Join(originCwd, ".claude", "worktrees", "wt")
	return Worktree{Cwd: p, Path: p, Name: "wt", Branch: "claude/wt", SourceBranch: "main"}, nil
}
func (f *fakeWorktrees) Remove(originCwd, path string) error { return f.removeErr }

type fakeLauncher struct{ agents []*fakeAgent }

func (f *fakeLauncher) Start(s *Session, out chan<- AgentEvent) (Agent, error) {
	ag := &fakeAgent{}
	f.agents = append(f.agents, ag)
	return ag, nil
}

type allowCall struct {
	requestID   string
	input       map[string]any
	permissions []any
}

// fakeAgent records what core asked of it; request ids are "req-<n>" in call order.
type fakeAgent struct {
	sent     []string
	requests []string
	allowed  []allowCall
}

func (f *fakeAgent) req(r string) string {
	f.requests = append(f.requests, r)
	return fmt.Sprintf("req-%d", len(f.requests))
}
func (f *fakeAgent) SendUser(text, uuid string) error        { f.sent = append(f.sent, text); return nil }
func (f *fakeAgent) Interrupt() string                       { return f.req("interrupt") }
func (f *fakeAgent) CancelQueued(uuid string) string         { return f.req("cancel " + uuid) }
func (f *fakeAgent) SetPermissionMode(mode string) string    { return f.req("mode " + mode) }
func (f *fakeAgent) SetModel(model string) string            { return f.req("model " + model) }
func (f *fakeAgent) SetEffort(effort string) string          { return f.req("effort " + effort) }
func (f *fakeAgent) Rename(title string) string              { return f.req("rename " + title) }
func (f *fakeAgent) GenerateTitle(description string) string { return f.req("title " + description) }
func (f *fakeAgent) Rewind(id string, dryRun bool) string {
	return f.req(fmt.Sprint("rewind ", id, dryRun))
}
func (f *fakeAgent) Allow(requestID string, input map[string]any, permissions []any) error {
	f.allowed = append(f.allowed, allowCall{requestID, input, permissions})
	return nil
}
func (f *fakeAgent) Deny(requestID, message string) error { return nil }
func (f *fakeAgent) Close()                               {}

type fakes struct {
	store     *fakeStore
	worktrees *fakeWorktrees
	launcher  *fakeLauncher
}

func testApp(t *testing.T) (*App, fakes) {
	t.Helper()
	f := fakes{&fakeStore{saved: map[string]Session{}}, &fakeWorktrees{}, &fakeLauncher{}}
	a, err := NewApp(f.store, fakeTranscripts{}, f.launcher, f.worktrees)
	if err != nil {
		t.Fatal(err)
	}
	return a, f
}

// withAgent gives s a running fake agent, as if a message had started it.
func withAgent(a *App, s *Session) *fakeAgent {
	ag := &fakeAgent{}
	a.L(s.SessionID).agent = ag
	return ag
}
