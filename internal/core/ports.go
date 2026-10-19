package core

import "time"

// SessionStore persists session records.
type SessionStore interface {
	Load() ([]*Session, error)
	Save(s *Session) error
	Delete(id string) error // os.ErrNotExist when there is nothing to delete
}

// Transcripts reads a session's conversation history from the agent's own record.
type Transcripts interface {
	Load(cwd, cliSessionID string) ([]Entry, error)
}

// Worktree is an isolated checkout a session runs in.
type Worktree struct{ Cwd, Path, Name, Branch, SourceBranch string }

type Worktrees interface {
	IsRepo(dir string) bool
	// Create cuts a worktree from HEAD of the repo holding originCwd, named after prompt.
	Create(originCwd, prompt string) (Worktree, error)
	// Remove must refuse a worktree with uncommitted changes.
	Remove(originCwd, path string) error
}

// Shells starts the terminal a session's side panel shows.
type Shells interface {
	// Open starts a shell in cwd on a w×h screen. It sends ShellChanged on out when the
	// screen changes, a few times a second at most, and ShellExited once when the shell ends.
	Open(sessionID, cwd string, w, h int, out chan<- AgentEvent) (Shell, error)
}

// Shell is one running shell and the screen its terminal shows.
type Shell interface {
	Key(k Key)
	Paste(text string)
	Resize(w, h int)
	Screen() *Screen
	Close()
}

// Key is one key press for a shell: the code, the text it types and its modifiers, as
// ultraviolet reports them.
type Key struct {
	Code rune   `json:"code"`
	Text string `json:"text,omitempty"`
	Mod  int    `json:"mod,omitempty"`
}

// Screen is what a shell's terminal shows: one ANSI-styled string per row, and the cursor.
type Screen struct {
	W, H     int
	Rows     []string
	CursorX  int
	CursorY  int
	CursorOn bool
}

// Backend is an agent harness a session can run on, and which optional features it has.
type Backend struct {
	Label           string // e.g. "Claude Code"
	Tag             string // session-list prefix, e.g. "cl"
	Launcher        Launcher
	Transcripts     Transcripts
	Rewind, Unqueue bool
}

// Caps is the part of a Backend clients hold: they send the commands, the process running
// the harness answers them.
type Caps struct {
	Label   string `json:"label"`
	Tag     string `json:"tag"`
	Rewind  bool   `json:"rewind"`
	Unqueue bool   `json:"unqueue"`
}

// Caps strips b to the part clients hold.
func (b Backend) Caps() Caps { return Caps{b.Label, b.Tag, b.Rewind, b.Unqueue} }

// DefaultBackend runs new sessions, and the sessions saved before there was a choice.
const DefaultBackend = "claude"

// Launcher starts an agent for s; the agent reports on out until it sends Exited.
type Launcher interface {
	// tools are this launch's Tools, as App.tools builds them.
	Start(s *Session, tools []map[string]any, out chan<- AgentEvent) (Agent, error)
	// Models lists the models an agent started in cwd offers. It blocks while it asks one.
	Models(cwd string) ([]Model, error)
}

// Model is one model a backend offers; ID is what SetModel takes.
type Model struct {
	ID, Name, Description string
	Efforts               []string // effort levels it accepts, none when it has no effort setting
}

// Agent is one running agent process. Methods returning a string send a request
// whose Reply arrives later as an event carrying that request id.
type Agent interface {
	// SendUser may be called mid-turn: the agent queues the message and reports
	// Consumed with this uuid once it reads it.
	SendUser(text, uuid string) error
	Interrupt() string
	CancelQueued(uuid string) string
	SetPermissionMode(mode string) string
	SetModel(model string) string
	SetEffort(effort string) string
	Rename(title string) string
	GenerateTitle(description string) string
	Rewind(userMessageID string, dryRun bool) string
	Allow(requestID string, input map[string]any, permissions []any) error
	Deny(requestID, message string) error
	Close()
}

// AgentEvent is one thing a session's agent reported.
type AgentEvent struct {
	SessionID string
	Event     Event
}

// Event is one of the types below.
type Event interface{ event() }

type (
	Initialized      struct{ Version, Model, CLISessionID string }
	Compacted        struct{}
	TextDelta        struct{ Text string }
	AssistantMessage struct{ Entries []Entry }
	UserMessage      struct{ Entries []Entry }
	// Consumed: the agent read a queued user message.
	Consumed  struct{ UUID string }
	TurnEnded struct {
		OK       bool
		Duration time.Duration
		Failure  string // why the turn failed, when !OK
	}
	PermissionRequested struct{ Perm *Perm }
	PermissionCancelled struct{ RequestID string }
	// TaskSuggested and TaskDismissed must be answered once through Reply.
	TaskSuggested struct {
		Chip  Chip
		Reply func(text string, isErr bool)
	}
	TaskDismissed struct {
		TaskID string
		Reply  func(text string, isErr bool)
	}
	// AgentRequested must be answered once through Reply.
	AgentRequested struct {
		Title, Prompt          string
		Backend, Model, Effort string // optional; see agentSetup
		Reply                  func(text string, isErr bool)
	}
	// Reply answers the request with RequestID; only the fields for that request are set.
	Reply struct {
		RequestID string
		Err       string
		Cancelled bool   // CancelQueued
		Title     string // GenerateTitle
		Rewind    RewindPreview
	}
	Exited struct{ Err error }
	// ShellChanged: the session's shell has new output on its screen.
	ShellChanged struct{}
	// ShellExited: Shell, the session's shell, ended.
	ShellExited struct{ Shell Shell }
	// ModelsListed carries a backend's model list.
	ModelsListed struct {
		Backend string
		Models  []Model
		Err     error
	}
)

func (Initialized) event()         {}
func (Compacted) event()           {}
func (TextDelta) event()           {}
func (AssistantMessage) event()    {}
func (UserMessage) event()         {}
func (Consumed) event()            {}
func (TurnEnded) event()           {}
func (PermissionRequested) event() {}
func (PermissionCancelled) event() {}
func (TaskSuggested) event()       {}
func (TaskDismissed) event()       {}
func (AgentRequested) event()      {}
func (Reply) event()               {}
func (Exited) event()              {}
func (ShellChanged) event()        {}
func (ShellExited) event()         {}
func (ModelsListed) event()        {}
