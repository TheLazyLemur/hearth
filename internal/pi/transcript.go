package pi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"hearth/internal/core"
)

var _ core.Transcripts = Transcripts{}

// Transcripts reads the session files pi keeps under Dir/--<cwd>--/<timestamp>_<id>.jsonl.
type Transcripts struct{ Dir string }

// Load finds the file by id alone: pi names the folder after the resolved cwd, which may differ from ours.
// shortcut: entries are read in file order, ignoring branches made by pi's /tree or fork; upgrade if those get used.
func (t Transcripts) Load(_, sessionID string) ([]core.Entry, error) {
	paths, err := filepath.Glob(filepath.Join(t.Dir, "*", "*_"+sessionID+".jsonl"))
	if err != nil {
		return nil, err
	}
	if len(paths) != 1 {
		return nil, fmt.Errorf("pi session %s: %d files found: %w", sessionID, len(paths), os.ErrNotExist)
	}
	f, err := os.Open(paths[0])
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
			if json.Unmarshal(line, &m) == nil && m["type"] == "message" {
				msg, _ := m["message"].(map[string]any)
				out = append(out, entries(msg)...)
			}
		}
		if err != nil {
			break
		}
	}
	return out, nil
}

// entries converts a pi message (session file or live stream, same shape) to entries.
func entries(msg map[string]any) []core.Entry {
	var out []core.Entry
	switch msg["role"] {
	case "user":
		if s, ok := msg["content"].(string); ok { // as one text block
			msg = map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
		}
		for _, b := range blocks(msg) {
			if b["type"] != "text" {
				continue
			}
			// an agent's report is hearth's message to the agent, not the user's: it shows as the agent's line
			if e, ok := core.ReportEntry(str(b["text"])); ok {
				out = append(out, e)
			} else {
				out = append(out, core.Entry{Kind: "user", Text: str(b["text"])})
			}
		}
	case "assistant":
		for _, b := range blocks(msg) {
			switch b["type"] {
			case "text":
				out = append(out, core.Entry{Kind: "assistant", Text: str(b["text"])})
			case "toolCall":
				args, _ := b["arguments"].(map[string]any)
				out = append(out, core.Entry{Kind: "tool", Text: str(b["name"]) + "(" + core.SummariseInput(args) + ")", ID: str(b["id"])})
			}
		}
	case "toolResult":
		var parts []string
		for _, b := range blocks(msg) {
			if b["type"] == "text" {
				parts = append(parts, str(b["text"]))
			}
		}
		out = append(out, core.Entry{Kind: "result", Text: core.FirstLines(strings.Join(parts, "\n"), 4),
			ID: str(msg["toolCallId"]), Err: msg["isError"] == true})
	}
	return out
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
