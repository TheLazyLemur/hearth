package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"hearth/internal/core"
)

var _ core.Transcripts = Transcripts{}

// Transcripts reads the JSONL transcripts the CLI keeps under ConfigDir/projects.
type Transcripts struct{ ConfigDir string }

// projectSlug matches the CLI's ~/.claude/projects directory naming: every
// non-alphanumeric character becomes '-'.
// shortcut: the CLI hashes very long paths; those transcripts won't be found, upgrade if it bites.
var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

func projectSlug(cwd string) string { return nonAlnum.ReplaceAllString(cwd, "-") }

func (t Transcripts) Load(cwd, cliSessionID string) ([]core.Entry, error) {
	return loadTranscript(filepath.Join(t.ConfigDir, "projects", projectSlug(cwd), cliSessionID+".jsonl"))
}

func loadTranscript(path string) ([]core.Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []core.Entry
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
func entriesFromMessage(m map[string]any) []core.Entry {
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
				return []core.Entry{{Kind: "interrupt"}}
			}
			return []core.Entry{{Kind: "user", Text: s, UUID: uuid}}
		}
		var out []core.Entry
		for _, b := range blocks(msg) {
			switch b["type"] {
			case "text":
				if t, _ := b["text"].(string); strings.HasPrefix(t, "[Request interrupted") {
					out = append(out, core.Entry{Kind: "interrupt"})
				} else if t != "" && !strings.HasPrefix(t, "<") {
					out = append(out, core.Entry{Kind: "user", Text: t, UUID: uuid})
				}
			case "tool_result":
				out = append(out, core.Entry{Kind: "result", Text: core.FirstLines(toolResultText(b["content"]), 4), ID: str(b["tool_use_id"]), Err: b["is_error"] == true})
			}
		}
		return out
	case "assistant":
		var out []core.Entry
		for _, b := range blocks(msg) {
			switch b["type"] {
			case "text":
				out = append(out, core.Entry{Kind: "assistant", Text: str(b["text"])})
			case "tool_use":
				in, _ := b["input"].(map[string]any)
				out = append(out, core.Entry{Kind: "tool", Text: str(b["name"]) + "(" + core.SummariseInput(in) + ")", ID: str(b["id"])})
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
