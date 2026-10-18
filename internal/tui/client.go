package tui

import "hearth/internal/core"

// Client is the app the TUI drives. It holds a copy of the daemon's state, kept current by
// Drain, and every command goes to the daemon, so several hearths can show the same sessions.
type Client interface {
	// Drain applies the changes the daemon sent since the last call, and reports whether
	// there were any. Read the copy after it, on the goroutine that draws.
	Drain() bool

	// Closed is closed when the daemon connection ends, and Err says why.
	Closed() <-chan struct{}
	Err() error
	// Close detaches, leaving the daemon and its agents running.
	Close()

	Sessions() []*core.Session
	Visible(showArchived bool) []*core.Session
	Find(id string) *core.Session
	L(id string) *core.Live
	Status(s *core.Session) string
	Backend(s *core.Session) core.Caps
	Efforts(s *core.Session) []string
	Models(s *core.Session) (models []core.Model, loading bool)
	EnsureLoaded(s *core.Session)
	AnyRunning() bool
	IsRepo(dir string) bool

	Notice() string
	SetNotice(text string)

	NewSession(dir string, worktree bool) *core.Session
	Delete(s *core.Session) error
	Rename(s *core.Session, title string)
	ToggleStar(s *core.Session)
	ToggleArchive(s *core.Session)
	SetModel(s *core.Session, model string)
	SetEffort(s *core.Session, effort string)
	SetMode(s *core.Session, mode string)
	SetBackend(s *core.Session, backend string) error

	Send(s *core.Session, text string) error
	Interrupt(s *core.Session)
	InterruptAndSend(s *core.Session, text string) error
	Unqueue(s *core.Session, onCancel func(string)) error

	PreviewRewind(s *core.Session, turn int, onPreview func(core.RewindPreview)) error
	Rewind(s *core.Session, turn int) error

	AnswerPerm(s *core.Session, allow, always bool) error
	AnswerQuestion(s *core.Session, text string) error

	AcceptChip(s *core.Session, i int) (*core.Session, error)
	DismissChip(s *core.Session, i int)
}
