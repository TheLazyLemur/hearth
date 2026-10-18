package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"hearth/internal/core"
)

type bufCloser struct{ bytes.Buffer }

func (b *bufCloser) Close() error { return nil }

func lastJSON(t *testing.T, b *bufCloser) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func decodeLine(t *testing.T, p *process, line string) core.Event {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatal(err)
	}
	return p.decode(m)
}

func TestLoadTranscriptBuildsEntriesFromCLIJSONL(t *testing.T) {
	// given
	// ... a CLI transcript with a user turn, a tool call, its result, a sidechain line, a reply and an agent's report
	path := filepath.Join(t.TempDir(), "s.jsonl")
	jsonl := `{"type":"queue-operation"}
{"type":"user","uuid":"u1","isSidechain":false,"message":{"role":"user","content":"hello"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"thinking","thinking":""},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","uuid":"u2","message":{"content":[{"type":"tool_result","content":"a\nb"}]}}
{"type":"assistant","uuid":"x","isSidechain":true,"message":{"content":[{"type":"text","text":"subagent"}]}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"done"}]}}
{"type":"user","uuid":"u3","message":{"role":"user","content":"<agent-report session=\"local_c1\" title=\"Find callers\" status=\"completed\">\n3 callers\n</agent-report>"}}
`
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}

	// when
	// ... the transcript is loaded
	es, err := loadTranscript(path)

	// then
	// ... entries follow the conversation, the user turn keeps its rewind UUID, sidechain is skipped, the report is the agent's line
	want := []core.Entry{{Kind: "user", Text: "hello", UUID: "u1"}, {Kind: "tool", Text: "Bash(ls)"}, {Kind: "result", Text: "a\nb"}, {Kind: "assistant", Text: "done"},
		{Kind: "info", Text: `↳ agent "Find callers" completed`, ID: "local_c1"}}
	if err != nil || !reflect.DeepEqual(es, want) {
		t.Fatalf("err %v entries %+v", err, es)
	}
}

func TestStreamLinesDecodeToCoreEvents(t *testing.T) {
	cases := []struct {
		name, line string
		want       core.Event
	}{
		{"init", `{"type":"system","subtype":"init","claude_code_version":"2.1","model":"opus","session_id":"c1"}`,
			core.Initialized{Version: "2.1", Model: "opus", CLISessionID: "c1"}},
		{"text delta", `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}}`,
			core.TextDelta{Text: "hi"}},
		{"replayed user message", `{"type":"user","isReplay":true,"uuid":"u1","message":{"role":"user","content":"x"}}`,
			core.Consumed{UUID: "u1"}},
		{"failed turn", `{"type":"result","subtype":"error_max_turns","duration_ms":2000,"errors":["boom"]}`,
			core.TurnEnded{Duration: 2e9, Failure: `error_max_turns ["boom"]`}},
		{"cancel reply", `{"type":"control_response","response":{"subtype":"success","request_id":"r1","response":{"cancelled":true}}}`,
			core.Reply{RequestID: "r1", Cancelled: true}},
		{"rewind preview", `{"type":"control_response","response":{"subtype":"success","request_id":"r2","response":{"canRewind":true,"filesChanged":["a","b"],"insertions":3,"deletions":1}}}`,
			core.Reply{RequestID: "r2", Rewind: core.RewindPreview{CanRewind: true, Files: 2, Insertions: 3, Deletions: 1}}},
		{"error reply", `{"type":"control_response","response":{"subtype":"error","request_id":"r3","error":"nope"}}`,
			core.Reply{RequestID: "r3", Err: "nope"}},
		{"permission cancelled", `{"type":"control_cancel_request","request_id":"r4"}`,
			core.PermissionCancelled{RequestID: "r4"}},
		{"subagent traffic", `{"type":"assistant","parent_tool_use_id":"t1","message":{"content":[]}}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// given
			// ... a CLI process
			p := &process{w: &bufCloser{}}

			// when
			// ... one stdout line is decoded
			got := decodeLine(t, p, c.line)

			// then
			// ... it becomes the matching core event
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v want %#v", got, c.want)
			}
		})
	}
}

func TestAlwaysAllowAnswersWithSuggestedPermissions(t *testing.T) {
	// given
	// ... the CLI asks to use Write with an acceptEdits suggestion
	w := &bufCloser{}
	p := &process{w: w}
	ev := decodeLine(t, p, `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Write",
		"input":{"file_path":"a.txt"},"permission_suggestions":[{"type":"setMode","mode":"acceptEdits","destination":"session"}]}}`)
	perm := ev.(core.PermissionRequested).Perm

	// when
	// ... it is allowed with the suggested permissions
	err := p.Allow(perm.RequestID, perm.Input, perm.Suggestions)

	// then
	// ... the CLI gets an allow carrying the input and the suggested permission update
	resp := lastJSON(t, w)["response"].(map[string]any)
	inner := resp["response"].(map[string]any)
	if err != nil || perm.Tool != "Write" || resp["request_id"] != "r1" || inner["behavior"] != "allow" ||
		inner["updatedInput"].(map[string]any)["file_path"] != "a.txt" || len(inner["updatedPermissions"].([]any)) != 1 {
		t.Fatalf("err %v perm %+v resp %+v", err, perm, resp)
	}
}

func TestSpawnTaskCallBecomesSuggestionAnsweredOverMCP(t *testing.T) {
	// given
	// ... the model calls spawn_task through the hearth MCP server
	w := &bufCloser{}
	p := &process{w: w}
	ev := decodeLine(t, p, `{"type":"control_request","request_id":"m1","request":{"subtype":"mcp_message","server_name":"hearth",
		"message":{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"spawn_task","arguments":{"title":"Fix stale docs","prompt":"Update README.md","tldr":"x"}}}}}`)
	task := ev.(core.TaskSuggested)

	// when
	// ... core answers the call
	task.Reply("Suggested.", false)

	// then
	// ... the chip carries the arguments and the JSON-RPC result answers request id 3
	resp := lastJSON(t, w)["response"].(map[string]any)
	mcp := resp["response"].(map[string]any)["mcp_response"].(map[string]any)
	if task.Chip != (core.Chip{Title: "Fix stale docs", Prompt: "Update README.md", Tldr: "x"}) ||
		resp["request_id"] != "m1" || mcp["id"] != 3.0 || mcp["result"].(map[string]any)["isError"] != false {
		t.Fatalf("chip %+v resp %+v", task.Chip, resp)
	}
}

func TestInitializeReplyListsModels(t *testing.T) {
	// given
	// ... the CLI's initialize reply with two models
	var resp map[string]any
	json.Unmarshal([]byte(`{"models":[{"value":"default","displayName":"Default (recommended)","description":"Opus 5.5 · Best for everyday, complex tasks","supportedEffortLevels":["low","high"]},
		{"value":"haiku","displayName":"Haiku","description":"Haiku 5.5 · Fastest"}]}`), &resp)

	// when
	// ... its models are read
	got := modelsFrom(resp)

	// then
	// ... each keeps its value as the id, with its name, description and effort levels
	want := []core.Model{{ID: "default", Name: "Default (recommended)", Description: "Opus 5.5 · Best for everyday, complex tasks", Efforts: []string{"low", "high"}},
		{ID: "haiku", Name: "Haiku", Description: "Haiku 5.5 · Fastest"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}
