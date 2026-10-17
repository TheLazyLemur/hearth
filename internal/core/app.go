package core

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"hearth/pkg/random"
)

// Live is per-session runtime state; none of it is persisted.
type Live struct {
	Entries      []Entry
	Running      bool
	Perms        []*Perm
	Chips        []*Chip
	Queued       []Entry   // sent to the agent, not yet consumed
	Started      time.Time // current turn, for the spinner
	Verb         string    // spinner verb for the current turn
	Version      string    // CLI version and model, from Initialized
	Model        string
	agent        Agent
	loaded       bool
	interrupted  bool // the running turn was interrupted by the user
	errored      bool
	pending      map[string]func(Reply)
	wantWorktree bool
}

type App struct {
	store     SessionStore
	backends  map[string]Backend
	models    map[string][]Model // by backend, once listed
	worktrees Worktrees
	sessions  []*Session
	live      map[string]*Live
	events    chan AgentEvent
	Notice    string
	now       func() int64
}

// NewApp needs backends to hold DefaultBackend.
func NewApp(store SessionStore, backends map[string]Backend, worktrees Worktrees) (*App, error) {
	if _, ok := backends[DefaultBackend]; !ok {
		panic("no " + DefaultBackend + " backend")
	}
	ss, err := store.Load()
	if err != nil {
		return nil, err
	}
	for _, s := range ss {
		if s.Backend == "" {
			s.Backend = DefaultBackend
		}
		if _, ok := backends[s.Backend]; !ok {
			return nil, fmt.Errorf("session %s uses unknown backend %q", s.SessionID, s.Backend)
		}
	}
	return &App{store: store, backends: backends, models: map[string][]Model{}, worktrees: worktrees, sessions: ss,
		live: map[string]*Live{}, events: make(chan AgentEvent, 256), now: func() int64 { return time.Now().UnixMilli() }}, nil
}

// Backend is the harness s runs on.
func (a *App) Backend(s *Session) Backend { return a.backends[s.Backend] }

// SetBackend picks the harness for a session that has not started yet.
func (a *App) SetBackend(s *Session, name string) error {
	if _, ok := a.backends[name]; !ok {
		names := slices.Sorted(maps.Keys(a.backends))
		return fmt.Errorf("backend: %s", strings.Join(names, "|"))
	}
	if s.CLISessionID != "" || a.L(s.SessionID).agent != nil {
		return fmt.Errorf("backend is fixed once the session has started")
	}
	s.Backend = name
	a.save(s)
	return nil
}

// Models lists the models s's backend offers. The first ask per backend lists them in the
// background and reports loading until they arrive.
func (a *App) Models(s *Session) (models []Model, loading bool) {
	ms, ok := a.models[s.Backend]
	if ok {
		return ms, ms == nil
	}
	a.models[s.Backend] = nil // listing
	b, launcher := s.Backend, a.Backend(s).Launcher
	go func() {
		ms, err := launcher.Models(s.Cwd)
		a.events <- AgentEvent{SessionID: s.SessionID, Event: ModelsListed{Backend: b, Models: ms, Err: err}}
	}()
	return nil, true
}

// Efforts lists the effort levels s's model accepts, or nil when its model isn't listed.
func (a *App) Efforts(s *Session) []string {
	id := cmp.Or(s.Model, a.L(s.SessionID).Model)
	for _, m := range a.models[s.Backend] {
		if m.ID == id {
			return m.Efforts
		}
	}
	return nil
}

func (a *App) unsupported(s *Session, what string) error {
	return fmt.Errorf("%s: not available with %s", what, a.Backend(s).Label)
}

// Events delivers agent reports; pass each to Handle on the goroutine that calls every other App method.
func (a *App) Events() <-chan AgentEvent { return a.events }

func (a *App) L(id string) *Live {
	l := a.live[id]
	if l == nil {
		l = &Live{pending: map[string]func(Reply){}}
		a.live[id] = l
	}
	return l
}

func (a *App) Find(id string) *Session {
	for _, s := range a.sessions {
		if s.SessionID == id {
			return s
		}
	}
	return nil
}

func (a *App) save(s *Session) {
	if err := a.store.Save(s); err != nil {
		a.Notice = "save failed: " + err.Error()
	}
}

func (a *App) IsRepo(dir string) bool { return a.worktrees.IsRepo(dir) }

// Visible lists sessions for the sidebar: starred first, then most recent.
func (a *App) Visible(showArchived bool) []*Session {
	var out []*Session
	for _, s := range a.sessions {
		if s.IsArchived == showArchived {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsStarred != out[j].IsStarred {
			return out[i].IsStarred
		}
		return out[i].LastActivityAt > out[j].LastActivityAt
	})
	return out
}

func (a *App) Status(s *Session) string {
	l := a.live[s.SessionID]
	switch {
	case l == nil:
		return "idle"
	case len(l.Perms) > 0:
		return "waiting"
	case l.Running:
		return "running"
	case l.errored:
		return "error"
	}
	return "idle"
}

func (a *App) AnyRunning() bool {
	for _, l := range a.live {
		if l.Running {
			return true
		}
	}
	return false
}

func newUUID() string {
	h := random.Hex(32)
	return h[:8] + "-" + h[8:12] + "-4" + h[13:16] + "-" + h[16:20] + "-" + h[20:]
}

func (a *App) NewSession(dir string, worktree bool) *Session {
	t := a.now()
	s := &Session{SessionID: "local_" + newUUID(), Backend: DefaultBackend, Cwd: dir, OriginCwd: dir, PermissionMode: "default",
		CreatedAt: t, LastActivityAt: t}
	a.L(s.SessionID).wantWorktree = worktree
	a.L(s.SessionID).loaded = true
	a.sessions = append([]*Session{s}, a.sessions...)
	a.save(s)
	return s
}

func (a *App) ToggleStar(s *Session) {
	s.IsStarred = !s.IsStarred
	a.save(s)
}

func (a *App) ToggleArchive(s *Session) {
	s.IsArchived = !s.IsArchived
	a.save(s)
}

func (a *App) EnsureLoaded(s *Session) {
	l := a.L(s.SessionID)
	if l.loaded || s.CLISessionID == "" {
		return
	}
	l.loaded = true
	es, err := a.Backend(s).Transcripts.Load(s.Cwd, s.CLISessionID)
	if err != nil {
		l.Entries = append(l.Entries, Entry{Kind: "error", Text: "transcript unavailable: " + err.Error()})
		return
	}
	l.Entries = es
}

func (a *App) ensureAgent(s *Session) (Agent, error) {
	l := a.L(s.SessionID)
	if l.agent != nil {
		return l.agent, nil
	}
	ag, err := a.Backend(s).Launcher.Start(s, a.events)
	if err != nil {
		return nil, err
	}
	l.agent, l.errored = ag, false
	return ag, nil
}

func (a *App) Send(s *Session, text string) error {
	l := a.L(s.SessionID)
	if l.wantWorktree {
		wt, err := a.worktrees.Create(s.OriginCwd, text)
		if err != nil {
			return err
		}
		s.Cwd, s.WorktreePath, s.WorktreeName, s.Branch, s.SourceBranch = wt.Cwd, wt.Path, wt.Name, wt.Branch, wt.SourceBranch
		l.wantWorktree = false
	}
	if s.Title == "" {
		s.Title, s.TitleSource = truncate(FirstLines(text, 1), 50), "auto"
	}
	ag, err := a.ensureAgent(s)
	if err != nil {
		return err
	}
	id := newUUID()
	if err := ag.SendUser(text, id); err != nil {
		return err
	}
	l.Queued = append(l.Queued, Entry{Kind: "user", Text: text, UUID: id})
	startTurn(l)
	s.LastActivityAt = a.now()
	a.save(s)
	return nil
}

// Interrupt stops the current turn; queued messages stay queued and run next.
func (a *App) Interrupt(s *Session) {
	if l := a.live[s.SessionID]; l != nil && l.agent != nil && l.Running {
		l.interrupted = true
		l.agent.Interrupt()
	}
}

// InterruptAndSend stops the current turn and sends text as the next one.
func (a *App) InterruptAndSend(s *Session, text string) error {
	a.Interrupt(s)
	return a.Send(s, text)
}

// Unqueue pulls the newest still-queued message back; done gets its text once the agent confirms.
func (a *App) Unqueue(s *Session, done func(string)) error {
	if !a.Backend(s).Unqueue {
		return a.unsupported(s, "unqueue")
	}
	l := a.L(s.SessionID)
	if len(l.Queued) == 0 || l.agent == nil {
		return fmt.Errorf("nothing queued")
	}
	e := l.Queued[len(l.Queued)-1]
	l.pending[l.agent.CancelQueued(e.UUID)] = func(r Reply) {
		if !r.Cancelled {
			a.Notice = "too late: the agent already read that message"
			return
		}
		for i, q := range l.Queued {
			if q.UUID == e.UUID {
				l.Queued = append(l.Queued[:i], l.Queued[i+1:]...)
				break
			}
		}
		done(e.Text)
	}
	return nil
}

// liveRequest pushes a setting to a running agent; the saved value applies at the next spawn regardless.
func (a *App) liveRequest(s *Session, req func(Agent) string) {
	if l := a.live[s.SessionID]; l != nil && l.agent != nil {
		l.pending[req(l.agent)] = func(Reply) {}
	}
	s.LastActivityAt = a.now()
	a.save(s)
}

func (a *App) SetMode(s *Session, mode string) {
	s.PermissionMode = mode
	a.liveRequest(s, func(ag Agent) string { return ag.SetPermissionMode(mode) })
}

func (a *App) SetModel(s *Session, model string) {
	s.Model = model
	a.liveRequest(s, func(ag Agent) string { return ag.SetModel(model) })
}

func (a *App) SetEffort(s *Session, effort string) {
	s.Effort = effort
	if effort != "" {
		a.liveRequest(s, func(ag Agent) string { return ag.SetEffort(effort) })
	} else {
		a.save(s)
	}
}

func (a *App) Rename(s *Session, title string) {
	s.Title, s.TitleSource = title, "user"
	if l := a.live[s.SessionID]; l != nil && l.agent != nil {
		l.pending[l.agent.Rename(title)] = func(Reply) {}
	}
	a.save(s)
}

// AnswerPerm resolves the oldest pending permission request for s.
func (a *App) AnswerPerm(s *Session, allow, always bool) error {
	l := a.L(s.SessionID)
	if len(l.Perms) == 0 || l.agent == nil {
		return fmt.Errorf("no pending permission request")
	}
	p := l.Perms[0]
	l.Perms = l.Perms[1:]
	if !allow {
		return l.agent.Deny(p.RequestID, "The user denied this tool use.")
	}
	input := p.Input
	if p.Answers != nil {
		input = map[string]any{}
		for k, v := range p.Input {
			input[k] = v
		}
		input["answers"] = p.Answers
	}
	var perms []any
	if always {
		perms = p.Suggestions
	}
	return l.agent.Allow(p.RequestID, input, perms)
}

// AnswerQuestion records text (an option number or free text) as the answer to the
// pending AskUserQuestion's next question, and submits once all are answered.
func (a *App) AnswerQuestion(s *Session, text string) error {
	l := a.L(s.SessionID)
	if len(l.Perms) == 0 || l.Perms[0].Answers == nil {
		return fmt.Errorf("no pending question")
	}
	p := l.Perms[0]
	qs := p.Questions()
	q := qs[len(p.Answers)]
	ans := text
	if n, err := strconv.Atoi(text); err == nil && n >= 1 && n <= len(q.Options) {
		ans = q.Options[n-1]
	}
	p.Answers[q.Question] = ans
	if len(p.Answers) == len(qs) {
		return a.AnswerPerm(s, true, false)
	}
	return nil
}

func (a *App) Delete(s *Session) error {
	l := a.L(s.SessionID)
	if l.agent != nil {
		l.agent.Close()
		l.agent = nil
	}
	if s.WorktreePath != "" {
		if err := a.worktrees.Remove(s.OriginCwd, s.WorktreePath); err != nil {
			return fmt.Errorf("kept session, worktree not removed: %w", err)
		}
	}
	if err := a.store.Delete(s.SessionID); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(a.live, s.SessionID)
	for i, x := range a.sessions {
		if x == s {
			a.sessions = append(a.sessions[:i], a.sessions[i+1:]...)
			break
		}
	}
	return nil
}

// AcceptChip starts the suggested task as its own session in a fresh worktree, like the desktop.
func (a *App) AcceptChip(parent *Session, i int) (*Session, error) {
	l := a.L(parent.SessionID)
	if i < 0 || i >= len(l.Chips) {
		return nil, fmt.Errorf("no task %d", i+1)
	}
	c := l.Chips[i]
	dir := c.Cwd
	if dir == "" {
		dir = parent.OriginCwd
	}
	s := a.NewSession(dir, a.worktrees.IsRepo(dir))
	s.Title, s.TitleSource = c.Title, "task"
	s.Backend, s.Model, s.Effort, s.PermissionMode = parent.Backend, parent.Model, parent.Effort, parent.PermissionMode
	s.SpawnedFrom = &SpawnedFrom{SessionID: parent.SessionID, TaskID: c.TaskID, Title: c.Title}
	l.Chips = append(l.Chips[:i], l.Chips[i+1:]...)
	return s, a.Send(s, c.Prompt)
}

func (a *App) DismissChip(s *Session, i int) {
	l := a.L(s.SessionID)
	if i >= 0 && i < len(l.Chips) {
		l.Chips = append(l.Chips[:i], l.Chips[i+1:]...)
	}
}

// RewindTargets lists user turns that have an agent UUID, in order.
func RewindTargets(l *Live) []Entry {
	var out []Entry
	for _, e := range l.Entries {
		if e.Kind == "user" && e.UUID != "" {
			out = append(out, e)
		}
	}
	return out
}

func (a *App) rewindTarget(s *Session, turn int) (string, error) {
	if !a.Backend(s).Rewind {
		return "", a.unsupported(s, "rewind")
	}
	targets := RewindTargets(a.L(s.SessionID))
	if turn < 1 || turn > len(targets) {
		return "", fmt.Errorf("turn must be 1-%d", len(targets))
	}
	return targets[turn-1].UUID, nil
}

// PreviewRewind asks what restoring files to before turn (1-based) would change.
func (a *App) PreviewRewind(s *Session, turn int, done func(RewindPreview)) error {
	id, err := a.rewindTarget(s, turn)
	if err != nil {
		return err
	}
	ag, err := a.ensureAgent(s)
	if err != nil {
		return err
	}
	a.L(s.SessionID).pending[ag.Rewind(id, true)] = func(r Reply) { done(r.Rewind) }
	return nil
}

// Rewind restores files to before turn (1-based).
func (a *App) Rewind(s *Session, turn int) error {
	id, err := a.rewindTarget(s, turn)
	if err != nil {
		return err
	}
	ag, err := a.ensureAgent(s)
	if err != nil {
		return err
	}
	l := a.L(s.SessionID)
	l.pending[ag.Rewind(id, false)] = func(Reply) {
		l.Entries = append(l.Entries, Entry{Kind: "info", Text: fmt.Sprintf("— files rewound to before turn %d —", turn)})
	}
	return nil
}

func (a *App) CloseAll() {
	for _, l := range a.live {
		if l.agent != nil {
			l.agent.Close()
		}
	}
}

// Handle applies one agent event to app state.
func (a *App) Handle(m AgentEvent) {
	if e, ok := m.Event.(ModelsListed); ok {
		if e.Err != nil {
			a.Notice = "listing models: " + e.Err.Error()
		}
		// shortcut: a failed listing caches as empty until restart, rather than retrying on every redraw.
		a.models[e.Backend] = append([]Model{}, e.Models...)
		return
	}
	s := a.Find(m.SessionID)
	if s == nil {
		return
	}
	l := a.L(s.SessionID)
	switch e := m.Event.(type) {
	case Exited:
		l.agent = nil
		l.Perms = nil
		for _, q := range l.Queued {
			l.Entries = append(l.Entries, Entry{Kind: "error", Text: "not delivered (" + a.Backend(s).Label + " exited): " + q.Text})
		}
		l.Queued = nil
		if l.Running {
			l.Running, l.errored = false, true
			l.Entries = append(l.Entries, Entry{Kind: "error", Text: fmt.Sprintf("%s exited: %v", a.Backend(s).Label, e.Err)})
		}
	case Initialized:
		l.Version, l.Model = e.Version, e.Model
		if e.CLISessionID != "" && e.CLISessionID != s.CLISessionID {
			s.CLISessionID = e.CLISessionID
			a.save(s)
		}
	case Compacted:
		l.Entries = append(l.Entries, Entry{Kind: "info", Text: "✻ Conversation compacted"})
	case TextDelta:
		if n := len(l.Entries); n > 0 && l.Entries[n-1].streaming {
			l.Entries[n-1].Text += e.Text
		} else {
			l.Entries = append(l.Entries, Entry{Kind: "assistant", Text: e.Text, streaming: true})
		}
	case AssistantMessage:
		for n := len(l.Entries); n > 0 && l.Entries[n-1].streaming; n-- {
			l.Entries = l.Entries[:n-1]
		}
		l.Entries = append(l.Entries, e.Entries...)
	case UserMessage:
		l.Entries = append(l.Entries, e.Entries...)
	case Consumed:
		a.deliver(l, e.UUID)
	case TurnEnded:
		l.Running = false
		s.CompletedTurns++
		s.LastActivityAt = a.now()
		interrupted := l.interrupted
		l.interrupted = false
		switch {
		case interrupted && !e.OK:
			// the agent already reported "[Request interrupted by user]", shown as an interrupt entry
		case e.OK:
			l.Entries = append(l.Entries, Entry{Kind: "info", Text: fmt.Sprintf("✻ %s for %s · done %s",
				doneVerbs[rand.IntN(len(doneVerbs))], FormatDuration(e.Duration), time.Now().Format("3:04 PM"))})
		default:
			l.Entries = append(l.Entries, Entry{Kind: "error", Text: e.Failure})
		}
		if s.TitleSource == "auto" && s.CompletedTurns == 1 {
			a.requestTitle(s, l)
		}
		a.save(s)
	case PermissionRequested:
		if l.agent == nil {
			panic("permission request from a session without an agent")
		}
		if e.Perm.Tool == "AskUserQuestion" {
			e.Perm.Answers = map[string]any{}
		}
		l.Perms = append(l.Perms, e.Perm)
	case PermissionCancelled:
		for i, p := range l.Perms {
			if p.RequestID == e.RequestID {
				l.Perms = append(l.Perms[:i], l.Perms[i+1:]...)
				break
			}
		}
	case TaskSuggested:
		c := e.Chip
		c.TaskID = "task_" + random.Hex(8)
		if c.Title == "" || c.Prompt == "" {
			e.Reply("title and prompt are required", true)
			return
		}
		l.Chips = append(l.Chips, &c)
		e.Reply(fmt.Sprintf("Suggested as %s. The user sees a chip and may start it; continue your turn.", c.TaskID), false)
	case TaskDismissed:
		for i, c := range l.Chips {
			if c.TaskID == e.TaskID {
				l.Chips = append(l.Chips[:i], l.Chips[i+1:]...)
				e.Reply("Withdrawn.", false)
				return
			}
		}
		e.Reply("That task was already started or dismissed; do not retry.", false)
	case Reply:
		cb, ok := l.pending[e.RequestID]
		if !ok {
			return // not ours, e.g. our own responses echoed back
		}
		delete(l.pending, e.RequestID)
		if e.Err != "" {
			a.Notice = a.Backend(s).Label + ": " + e.Err
			return
		}
		cb(e)
	default:
		panic(fmt.Sprintf("unhandled agent event %T", e))
	}
}

// startTurn marks a turn running; a message steered into a running turn keeps its spinner.
func startTurn(l *Live) {
	if !l.Running {
		l.Running, l.Started, l.Verb = true, time.Now(), spinnerVerbs[rand.IntN(len(spinnerVerbs))]
	}
}

// FormatDuration matches the CLI's "9s" / "1m 5s" turn timings.
func FormatDuration(d time.Duration) string {
	sec := int(d.Round(time.Second) / time.Second)
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%dm %ds", sec/60, sec%60)
}

// deliver moves a queued message into the transcript at the point the agent consumed it.
func (a *App) deliver(l *Live, uuid string) {
	for i, q := range l.Queued {
		if q.UUID == uuid {
			l.Queued = append(l.Queued[:i], l.Queued[i+1:]...)
			l.Entries = append(l.Entries, q)
			startTurn(l)
			return
		}
	}
}

func (a *App) requestTitle(s *Session, l *Live) {
	var first string
	for _, e := range l.Entries {
		if e.Kind == "user" {
			first = e.Text
			break
		}
	}
	if first == "" || l.agent == nil {
		return
	}
	l.pending[l.agent.GenerateTitle(first)] = func(r Reply) {
		if r.Title != "" && s.TitleSource == "auto" {
			s.Title = r.Title
			a.save(s)
		}
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
