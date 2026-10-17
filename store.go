package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Session mirrors the desktop app's claude-code-sessions/<acct>/<org>/local_<uuid>.json
// shape (subset) so the two stay easy to compare. The transcript itself lives
// in the CLI's ~/.claude/projects/<slug>/<cliSessionId>.jsonl.
type Session struct {
	SessionID      string       `json:"sessionId"`
	CLISessionID   string       `json:"cliSessionId,omitempty"`
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
}

type Store struct{ dir string }

func defaultStore() Store {
	if d := os.Getenv("CLAUDE_TUI_HOME"); d != "" {
		return Store{filepath.Join(d, "sessions")}
	}
	home, _ := os.UserHomeDir()
	return Store{filepath.Join(home, ".claude-tui", "sessions")}
}

func (s Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s Store) Load() ([]*Session, error) {
	files, err := filepath.Glob(filepath.Join(s.dir, "local_*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var ss Session
		if err := json.Unmarshal(b, &ss); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, &ss)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActivityAt > out[j].LastActivityAt })
	return out, nil
}

// Save writes atomically so a crash mid-write never leaves a truncated session file.
func (s Store) Save(ss *Session) error {
	if !strings.HasPrefix(ss.SessionID, "local_") {
		panic("session id must start with local_: " + ss.SessionID)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ss, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(ss.SessionID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(ss.SessionID))
}

func (s Store) Delete(id string) error { return os.Remove(s.path(id)) }

// projectSlug matches the CLI's ~/.claude/projects directory naming: every
// non-alphanumeric character becomes '-'.
// shortcut: the CLI hashes very long paths; those transcripts won't be found, upgrade if it bites.
var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

func projectSlug(cwd string) string { return nonAlnum.ReplaceAllString(cwd, "-") }

func transcriptPath(cwd, cliID string) string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, "projects", projectSlug(cwd), cliID+".jsonl")
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

func loadTranscript(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var m map[string]any
			if json.Unmarshal(line, &m) == nil && m["isSidechain"] != true && m["isMeta"] != true {
				out = append(out, entriesFromMessage(m)...)
			}
		}
		if err != nil {
			break
		}
	}
	return out, nil
}

// entriesFromMessage converts a user/assistant record (JSONL or stream-json, same shape) to entries.
func entriesFromMessage(m map[string]any) []Entry {
	msg, _ := m["message"].(map[string]any)
	if msg == nil {
		return nil
	}
	uuid, _ := m["uuid"].(string)
	switch m["type"] {
	case "user":
		if s, ok := msg["content"].(string); ok {
			if strings.HasPrefix(s, "<") { // command/system wrappers, not typed by the user
				return nil
			}
			if strings.HasPrefix(s, "[Request interrupted") {
				return []Entry{{Kind: "interrupt"}}
			}
			return []Entry{{Kind: "user", Text: s, UUID: uuid}}
		}
		var out []Entry
		for _, b := range blocks(msg) {
			switch b["type"] {
			case "text":
				if t, _ := b["text"].(string); strings.HasPrefix(t, "[Request interrupted") {
					out = append(out, Entry{Kind: "interrupt"})
				} else if t != "" && !strings.HasPrefix(t, "<") {
					out = append(out, Entry{Kind: "user", Text: t, UUID: uuid})
				}
			case "tool_result":
				out = append(out, Entry{Kind: "result", Text: firstLines(toolResultText(b["content"]), 4), ID: str(b["tool_use_id"]), Err: b["is_error"] == true})
			}
		}
		return out
	case "assistant":
		var out []Entry
		for _, b := range blocks(msg) {
			switch b["type"] {
			case "text":
				out = append(out, Entry{Kind: "assistant", Text: str(b["text"])})
			case "tool_use":
				in, _ := b["input"].(map[string]any)
				out = append(out, Entry{Kind: "tool", Text: str(b["name"]) + "(" + summariseInput(in) + ")", ID: str(b["id"])})
			}
		}
		return out
	}
	return nil
}

func blocks(msg map[string]any) []map[string]any {
	arr, _ := msg["content"].([]any)
	var out []map[string]any
	for _, a := range arr {
		if b, ok := a.(map[string]any); ok {
			out = append(out, b)
		}
	}
	return out
}

func toolResultText(c any) string {
	if s, ok := c.(string); ok {
		return s
	}
	var parts []string
	if arr, ok := c.([]any); ok {
		for _, a := range arr {
			if b, ok := a.(map[string]any); ok && b["type"] == "text" {
				parts = append(parts, str(b["text"]))
			}
		}
	}
	return strings.Join(parts, "\n")
}

func summariseInput(in map[string]any) string {
	for _, k := range []string{"command", "file_path", "pattern", "url", "query", "description", "prompt"} {
		if v, ok := in[k].(string); ok && v != "" {
			return firstLines(v, 1)
		}
	}
	b, _ := json.Marshal(in)
	return firstLines(string(b), 1)
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… +%d lines", len(lines)-n)
	}
	return strings.Join(lines, "\n")
}

func str(v any) string { s, _ := v.(string); return s }
