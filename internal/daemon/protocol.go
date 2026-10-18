// Package daemon runs the one process that owns a hearth app and serves its state to
// every TUI attached to it, over newline-delimited JSON on a Unix socket.
package daemon

import (
	"encoding/json"
	"path/filepath"
	"time"

	"hearth/internal/core"
)

// Version is the protocol version; a client and a daemon must speak the same one.
const Version = "1"

// noSession is the error for a request that names a session the daemon no longer has, which
// happens when another client deleted it first.
const noSession = "no such session"

// SocketPath is the daemon's socket, LockPath its flock and LogPath the file autostart
// sends the daemon's output to. All three live under HEARTH_HOME.
func SocketPath(home string) string { return filepath.Join(home, "daemon.sock") }
func LockPath(home string) string   { return filepath.Join(home, "daemon.lock") }
func LogPath(home string) string    { return filepath.Join(home, "daemon.log") }

// msg is one line of the protocol; which field is set says what it is.
type msg struct {
	Hello    *hello    `json:"hello,omitempty"`
	Snapshot *snapshot `json:"snapshot,omitempty"`
	Change   *change   `json:"change,omitempty"`
	Request  *request  `json:"request,omitempty"`
	Reply    *reply    `json:"reply,omitempty"`
	Bye      string    `json:"bye,omitempty"`
}

// hello is the client's first line, and the daemon's answer to a version it can't serve.
type hello struct {
	Version string `json:"version"`
	Busy    bool   `json:"busy,omitempty"`
}

// snapshot is everything a new client needs to start: every session, the plain data and
// entries of the loaded ones, each backend's capabilities, the model lists fetched so far
// and the daemon's notice.
type snapshot struct {
	Sessions []*core.Session         `json:"sessions"`
	Live     map[string]*core.Live   `json:"live"`
	Backends map[string]core.Caps    `json:"backends"`
	Models   map[string][]core.Model `json:"models"`
	Notice   string                  `json:"notice"`
}

// change is what a client is missing of a session: its record, its plain data without
// entries, and the entries from index splice.From on. Deletion, model listings and the
// notice are changes of their own.
type change struct {
	Session *core.Session `json:"session,omitempty"`
	Live    *core.Live    `json:"live,omitempty"`
	Entries *splice       `json:"entries,omitempty"`
	Deleted string        `json:"deleted,omitempty"`
	Models  *models       `json:"models,omitempty"`
	Notice  *string       `json:"notice,omitempty"`
}

// splice means "replace the session's entries from index From on with Items".
type splice struct {
	From  int          `json:"from"`
	Items []core.Entry `json:"items"`
}

// models is one backend's model list; nil Items while its listing runs.
type models struct {
	Backend string       `json:"backend"`
	Items   []core.Model `json:"items"`
}

// request is one command from a client.
type request struct {
	ID     int    `json:"id"`
	Method string `json:"method"`
	Args   args   `json:"args,omitempty"`
}

// args holds every request's parameters; each method reads the ones it uses.
type args struct {
	Session  string `json:"session,omitempty"`
	Text     string `json:"text,omitempty"`
	Title    string `json:"title,omitempty"`
	Model    string `json:"model,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Backend  string `json:"backend,omitempty"`
	Dir      string `json:"dir,omitempty"`
	Turn     int    `json:"turn,omitempty"`
	Index    int    `json:"index,omitempty"`
	Allow    bool   `json:"allow,omitempty"`
	Always   bool   `json:"always,omitempty"`
	Worktree bool   `json:"worktree,omitempty"`
}

// reply answers a request. Result is small, never state, and only some methods set it.
type reply struct {
	ID     int             `json:"id"`
	Err    string          `json:"err,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Status is what `hearth daemon status` prints.
type Status struct {
	PID     int
	Started time.Time
	Version string
	Clients int
	Running []string // titles of the sessions a stop would cut short
}

// VersionError reports a daemon that speaks another version than the client expects.
type VersionError struct {
	Version string
	Busy    bool // the daemon is running turns, so it must not be replaced
}

func (e *VersionError) Error() string {
	if e.Busy {
		return "daemon v" + e.Version + " is running turns; finish or `hearth daemon stop`"
	}
	return "daemon v" + e.Version + " is idle"
}
