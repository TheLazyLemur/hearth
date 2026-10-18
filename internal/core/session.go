package core

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Session mirrors the desktop app's claude-code-sessions/<acct>/<org>/local_<uuid>.json
// shape (subset) so the two stay easy to compare. The transcript itself lives
// in the CLI's ~/.claude/projects/<slug>/<cliSessionId>.jsonl.
type Session struct {
	SessionID      string       `json:"sessionId"`
	CLISessionID   string       `json:"cliSessionId,omitempty"`
	Backend        string       `json:"backend,omitempty"` // a key of the App's backends
	Cwd            string       `json:"cwd"`
	OriginCwd      string       `json:"originCwd"`
	Title          string       `json:"title"`
	TitleSource    string       `json:"titleSource,omitempty"` // "auto" | "user"
	IsArchived     bool         `json:"isArchived"`
	IsStarred      bool         `json:"isStarred,omitempty"`
	Model          string       `json:"model,omitempty"`
	Effort         string       `json:"effort,omitempty"`
	PermissionMode string       `json:"permissionMode"`
	Branch         string       `json:"branch,omitempty"`
	SourceBranch   string       `json:"sourceBranch,omitempty"`
	WorktreePath   string       `json:"worktreePath,omitempty"`
	WorktreeName   string       `json:"worktreeName,omitempty"`
	CreatedAt      int64        `json:"createdAt"`
	LastActivityAt int64        `json:"lastActivityAt"`
	CompletedTurns int          `json:"completedTurns"`
	SpawnedFrom    *SpawnedFrom `json:"spawnedFrom,omitempty"`
}

type SpawnedFrom struct {
	SessionID string `json:"sessionId"`
	TaskID    string `json:"taskId"`
	Title     string `json:"title"`
	Kind      string `json:"kind,omitempty"` // "agent": started by the parent's model with start_agent; "": an accepted task chip
}

// Entry is one rendered transcript item, built from either the JSONL file or the live stream.
type Entry struct {
	Kind      string // user | assistant | tool | result | info | interrupt | error
	Text      string
	UUID      string // user messages only: the rewind_files target
	ID        string // tool and result: the tool_use id pairing them
	Err       bool   // result: the tool failed or was denied
	streaming bool
}

// Perm is a pending can_use_tool request awaiting the user's answer.
type Perm struct {
	RequestID   string
	Tool        string
	Input       map[string]any
	Suggestions []any
	Reason      string         // why it asks, when a classifier flagged the call
	Answers     map[string]any // AskUserQuestion: question text -> answer
}

type Question struct {
	Question string
	Options  []string
}

// Questions lists an AskUserQuestion request's questions and their option labels.
func (p *Perm) Questions() []Question {
	var out []Question
	qs, _ := p.Input["questions"].([]any)
	for _, q := range qs {
		qm, _ := q.(map[string]any)
		var opts []string
		os, _ := qm["options"].([]any)
		for _, o := range os {
			om, _ := o.(map[string]any)
			opts = append(opts, str(om["label"]))
		}
		out = append(out, Question{str(qm["question"]), opts})
	}
	return out
}

// Chip is a task suggestion the model raised with spawn_task.
type Chip struct{ TaskID, Title, Prompt, Tldr, Cwd string }

// RewindPreview is what rewinding files to before a turn would restore.
type RewindPreview struct {
	CanRewind             bool
	Error                 string
	Files                 int
	Insertions, Deletions int
}

// SummariseInput picks the one tool argument that says what a tool call does.
func SummariseInput(in map[string]any) string {
	for _, k := range []string{"command", "file_path", "pattern", "url", "query", "description", "prompt"} {
		if v, ok := in[k].(string); ok && v != "" {
			return FirstLines(v, 1)
		}
	}
	b, _ := json.Marshal(in)
	return FirstLines(string(b), 1)
}

func FirstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… +%d lines", len(lines)-n)
	}
	return strings.Join(lines, "\n")
}

func str(v any) string { s, _ := v.(string); return s }
