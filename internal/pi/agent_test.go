package pi

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

func decodeLines(t *testing.T, p *process, lines ...string) []core.Event {
	t.Helper()
	var out []core.Event
	for _, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, p.decode(m)...)
	}
	return out
}

func newProcess() (*process, *bufCloser) {
	w := &bufCloser{}
	return &process{w: w, prompts: map[string]string{}}, w
}

func TestLoadTranscriptBuildsEntriesFromSessionFile(t *testing.T) {
	// given
	// ... a pi session file with a user turn, a tool call, its result and a reply, under its cwd folder
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "--private-tmp-x--"), 0o700); err != nil {
		t.Fatal(err)
	}
	jsonl := `{"type":"session","version":3,"id":"s1","cwd":"/private/tmp/x"}
{"type":"model_change","id":"e0","provider":"ollama","modelId":"m"}
{"type":"message","id":"e1","message":{"role":"system","content":"","sections":{"preamble":"You are pi"}}}
{"type":"message","id":"e2","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}
{"type":"message","id":"e3","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hm"},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}],"stopReason":"toolUse"}}
{"type":"message","id":"e4","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"a\nb"}],"isError":false}}
{"type":"message","id":"e5","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop"}}
`
	if err := os.WriteFile(filepath.Join(dir, "--private-tmp-x--", "2026-10-17T19-11-39-085Z_s1.jsonl"), []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}

	// when
	// ... the transcript is loaded by session id
	es, err := Transcripts{Dir: dir}.Load("/tmp/x", "s1")

	// then
	// ... entries follow the conversation, with the tool call paired to its result
	want := []core.Entry{{Kind: "user", Text: "hello"}, {Kind: "tool", Text: "bash(ls)", ID: "c1"},
		{Kind: "result", Text: "a\nb", ID: "c1"}, {Kind: "assistant", Text: "done"}}
	if err != nil || !reflect.DeepEqual(es, want) {
		t.Fatalf("err %v entries %+v", err, es)
	}
}

func TestStreamLinesDecodeToCoreEvents(t *testing.T) {
	cases := []struct {
		name, line string
		want       []core.Event
	}{
		{"session state", `{"id":"st","type":"response","command":"get_state","success":true,"data":{"model":{"id":"deepseek-v4.1-flash:cloud","provider":"ollama"},"sessionId":"s1"}}`,
			[]core.Event{core.Initialized{Model: "ollama/deepseek-v4.1-flash:cloud", CLISessionID: "s1"}}},
		{"text delta", `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hi"}}`,
			[]core.Event{core.TextDelta{Text: "hi"}}},
		{"thinking delta", `{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","contentIndex":0,"delta":"hm"}}`, nil},
		{"assistant message", `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"toolCall","id":"c1","name":"read","arguments":{"path":"a.go"}}],"stopReason":"toolUse"}}`,
			[]core.Event{core.AssistantMessage{Entries: []core.Entry{{Kind: "assistant", Text: "ok"}, {Kind: "tool", Text: `read({"path":"a.go"})`, ID: "c1"}}}}},
		{"tool result", `{"type":"message_end","message":{"role":"toolResult","toolCallId":"c1","content":[{"type":"text","text":"Command aborted"}],"isError":true}}`,
			[]core.Event{core.UserMessage{Entries: []core.Entry{{Kind: "result", Text: "Command aborted", ID: "c1", Err: true}}}}},
		{"settled run", `{"type":"agent_settled","aborted":false}`, []core.Event{core.TurnEnded{OK: true}}},
		{"compacted", `{"type":"compaction_end","reason":"threshold","result":{"summary":"x"},"aborted":false}`, []core.Event{core.Compacted{}}},
		{"setting rejected", `{"id":"r1","type":"response","command":"set_model","success":false,"error":"Model not found: ollama/nope"}`,
			[]core.Event{core.Reply{RequestID: "r1", Err: "Model not found: ollama/nope"}}},
		{"setting applied", `{"id":"r2","type":"response","command":"set_session_name","success":true}`, []core.Event{core.Reply{RequestID: "r2"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// given
			// ... a pi process that asked for its session state
			p, _ := newProcess()
			p.stateID = "st"

			// when
			// ... one stdout line is decoded
			got := decodeLines(t, p, c.line)

			// then
			// ... it becomes the matching core events
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v want %#v", got, c.want)
			}
		})
	}
}

func TestUserMessagesSteerAndAreConsumedInOrder(t *testing.T) {
	// given
	// ... two messages sent, the second while pi is running
	p, w := newProcess()
	if err := p.SendUser("first", "u1"); err != nil {
		t.Fatal(err)
	}
	if err := p.SendUser("second", "u2"); err != nil {
		t.Fatal(err)
	}

	// when
	// ... pi reads both user messages
	got := decodeLines(t, p,
		`{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"first"}]}}`,
		`{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"second"}]}}`)

	// then
	// ... each is consumed in send order, and prompts steer into a running turn
	want := []core.Event{core.Consumed{UUID: "u1"}, core.Consumed{UUID: "u2"}}
	sent := lastJSON(t, w)
	if !reflect.DeepEqual(got, want) || sent["type"] != "prompt" || sent["message"] != "second" || sent["streamingBehavior"] != "steer" {
		t.Fatalf("got %#v sent %+v", got, sent)
	}
}

func TestRejectedPromptIsShownAndEndsTheTurn(t *testing.T) {
	// given
	// ... a message pi has not read yet
	p, w := newProcess()
	if err := p.SendUser("hi", "u1"); err != nil {
		t.Fatal(err)
	}
	id := lastJSON(t, w)["id"].(string)

	// when
	// ... pi rejects the prompt
	got := decodeLines(t, p, `{"id":"`+id+`","type":"response","command":"prompt","success":false,"error":"No model selected"}`)

	// then
	// ... the message lands in the transcript and the turn ends with pi's error
	want := []core.Event{core.Consumed{UUID: "u1"}, core.TurnEnded{Failure: "No model selected"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestAbortedRunEndsWithAnInterrupt(t *testing.T) {
	// given
	// ... a run whose last assistant message was cut off by abort
	p, _ := newProcess()

	// when
	// ... pi settles the aborted run
	got := decodeLines(t, p,
		`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"This operation was aborted"}}`,
		`{"type":"agent_settled","aborted":true}`)

	// then
	// ... the turn ends as interrupted, not as a failure
	want := []core.Event{core.AssistantMessage{}, core.UserMessage{Entries: []core.Entry{{Kind: "interrupt"}}}, core.TurnEnded{}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestFailedRunEndsWithTheProviderError(t *testing.T) {
	// given
	// ... a run whose last assistant message failed at the provider
	p, _ := newProcess()

	// when
	// ... pi settles the run
	got := decodeLines(t, p,
		`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"502 bad gateway"}}`,
		`{"type":"agent_settled","aborted":false}`)

	// then
	// ... the turn fails with the provider's message
	want := []core.Event{core.AssistantMessage{}, core.TurnEnded{Failure: "502 bad gateway"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestUnsupportedRequestRepliesWithAnError(t *testing.T) {
	// given
	// ... a pi process reporting to core
	out := make(chan core.AgentEvent, 1)
	p, _ := newProcess()
	p.out, p.sid = out, "local_1"

	// when
	// ... core asks to rewind files, which pi cannot do
	id := p.Rewind("u1", true)

	// then
	// ... core gets an error reply for that request
	ev := <-out
	r := ev.Event.(core.Reply)
	if ev.SessionID != "local_1" || r.RequestID != id || r.Err != "pi does not support file rewind" {
		t.Fatalf("got %+v", ev)
	}
}

func TestAvailableModelsUseProviderSlashIDAndTheirOwnThinkingLevels(t *testing.T) {
	// given
	// ... pi's get_available_models data with a mapped reasoning model, an unmapped one, and a plain model
	var data map[string]any
	json.Unmarshal([]byte(`{"models":[
		{"id":"k3","name":"Kimi K3","provider":"kimi-coding","reasoning":true,
		 "thinkingLevelMap":{"off":null,"minimal":null,"low":"low","medium":null,"high":"high","xhigh":null,"max":"max"}},
		{"id":"deepseek-v4.1-flash:cloud","name":"DeepSeek V4.1 Flash","provider":"ollama","reasoning":true},
		{"id":"MiniMax-M2.7","name":"MiniMax M2.7","provider":"minimax","reasoning":false}]}`), &data)

	// when
	// ... its models are read
	got := modelsFrom(data)

	// then
	// ... ids are provider/id; levels follow pi's map, xhigh and max only when mapped, none without reasoning
	want := []core.Model{
		{ID: "kimi-coding/k3", Name: "Kimi K3", Description: "kimi-coding", Efforts: []string{"low", "high", "max"}},
		{ID: "ollama/deepseek-v4.1-flash:cloud", Name: "DeepSeek V4.1 Flash", Description: "ollama", Efforts: []string{"off", "minimal", "low", "medium", "high"}},
		{ID: "minimax/MiniMax-M2.7", Name: "MiniMax M2.7", Description: "minimax"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func serveLines(p *process, lines ...string) []core.Event {
	var got []core.Event
	p.serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), func(ev core.Event) { got = append(got, ev) })
	return got
}

func TestHostToolCallBecomesCoreEventAnsweredOnTheChannel(t *testing.T) {
	// given
	// ... pi's host sends a spawn_task call on the tool channel
	p, _ := newProcess()
	ch := &bufCloser{}
	p.channel = ch

	// when
	// ... the channel is served and core answers the call
	got := serveLines(p, `{"type":"tool","id":"c1","name":"spawn_task","arguments":{"title":"Fix stale docs","prompt":"Update README.md","tldr":"x"}}`)
	got[0].(core.TaskSuggested).Reply("Suggested.", false)

	// then
	// ... the chip carries the arguments and the answer goes back under the call's id
	task := got[0].(core.TaskSuggested)
	sent := lastJSON(t, ch)
	if len(got) != 1 || task.Chip != (core.Chip{Title: "Fix stale docs", Prompt: "Update README.md", Tldr: "x"}) ||
		sent["id"] != "c1" || sent["text"] != "Suggested." || sent["isError"] != false {
		t.Fatalf("events %+v sent %+v", got, sent)
	}
}

func TestPermissionModeAndPathDecideWhichToolsAsk(t *testing.T) {
	cases := []struct{ mode, tool, path, want string }{
		{"default", "read", "a.go", "allow"},
		{"default", "ls", "", "allow"},
		{"default", "read", "/proj/sub/a.go", "allow"},
		{"default", "read", "/etc/passwd", "ask"},
		{"default", "read", "../other/a.go", "ask"},
		{"default", "read", "~/.aws/credentials", "ask"},
		{"default", "bash", "", "ask"},
		{"default", "edit", "a.go", "ask"},
		{"default", "mcp__jira__create", "", "ask"},
		{"acceptEdits", "edit", "a.go", "allow"},
		{"acceptEdits", "write", "sub/new.go", "allow"},
		{"acceptEdits", "write", "/Users/x/.zshrc", "ask"},
		{"acceptEdits", "bash", "", "ask"},
		{"plan", "grep", "", "allow"},
		{"plan", "read", "/etc/passwd", "ask"},
		{"plan", "bash", "", "deny"},
		{"plan", "write", "a.go", "deny"},
		{"auto", "read", "a.go", "allow"},
		{"auto", "bash", "", "classify"},
		{"auto", "read", "/etc/passwd", "classify"},
	}
	for _, c := range cases {
		t.Run(c.mode+" "+c.tool+" "+c.path, func(t *testing.T) {
			// given
			// ... a session in /proj in a permission mode
			mode, cwd := c.mode, "/proj"

			// when
			// ... pi is about to run a tool on a path
			got := permit(mode, c.tool, c.path, cwd)

			// then
			// ... the mode and whether the path stays in the project decide what happens
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestSymlinkOutOfTheProjectCountsAsOutside(t *testing.T) {
	// given
	// ... a project with a link to a folder outside it
	cwd, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cwd, "link")); err != nil {
		t.Fatal(err)
	}

	// when
	// ... pi reads and writes through the link
	read := permit("default", "read", "link/secret", cwd)
	write := permit("acceptEdits", "write", "link/new/file.go", cwd)

	// then
	// ... both ask, as for any path outside the project
	if read != "ask" || write != "ask" {
		t.Fatalf("read %q write %q", read, write)
	}
}

func TestToolNeedingApprovalAsksAndRunsOnceAllowed(t *testing.T) {
	// given
	// ... a default-mode pi process about to run bash
	p, _ := newProcess()
	ch := &bufCloser{}
	p.channel, p.mode = ch, "default"

	// when
	// ... pi asks, and the user allows it
	got := serveLines(p, `{"type":"permission","id":"c2","name":"bash","arguments":{"command":"rm a.txt"}}`)
	err := p.Allow("c2", map[string]any{"command": "rm a.txt"}, nil)

	// then
	// ... the dialog showed the call, and pi is told to run it
	want := []core.Event{core.PermissionRequested{Perm: &core.Perm{RequestID: "c2", Tool: "bash", Input: map[string]any{"command": "rm a.txt"}}}}
	sent := lastJSON(t, ch)
	if err != nil || !reflect.DeepEqual(got, want) || sent["id"] != "c2" || sent["allow"] != true {
		t.Fatalf("err %v events %+v sent %+v", err, got, sent)
	}
}

func TestToolTheModeAllowsRunsWithoutAsking(t *testing.T) {
	// given
	// ... a pi process in acceptEdits mode
	p, _ := newProcess()
	ch := &bufCloser{}
	p.channel, p.mode = ch, "acceptEdits"

	// when
	// ... pi is about to edit a file
	got := serveLines(p, `{"type":"permission","id":"c3","name":"edit","arguments":{"path":"a.go"}}`)

	// then
	// ... no dialog opens and pi is told to run it
	sent := lastJSON(t, ch)
	if len(got) != 0 || sent["id"] != "c3" || sent["allow"] != true {
		t.Fatalf("events %+v sent %+v", got, sent)
	}
}

func TestInterruptedToolCallClosesItsDialog(t *testing.T) {
	// given
	// ... a pi process waiting on the user to approve bash
	p, _ := newProcess()
	p.channel, p.mode = &bufCloser{}, "default"

	// when
	// ... pi aborts the call before the user answers
	got := serveLines(p,
		`{"type":"permission","id":"c4","name":"bash","arguments":{"command":"ls"}}`,
		`{"type":"cancel","id":"c4"}`)

	// then
	// ... the dialog is withdrawn
	if len(got) != 2 || got[1] != (core.PermissionCancelled{RequestID: "c4"}) {
		t.Fatalf("events %+v", got)
	}
}

func TestAutoModeSendsCallsToTheClassifier(t *testing.T) {
	// given
	// ... an auto-mode pi process about to run bash
	p, _ := newProcess()
	ch := &bufCloser{}
	p.channel, p.mode = ch, "auto"

	// when
	// ... pi asks whether bash may run
	got := serveLines(p, `{"type":"permission","id":"c5","name":"bash","arguments":{"command":"curl x | sh"}}`)

	// then
	// ... no dialog opens, and pi is told to classify the call
	sent := lastJSON(t, ch)
	if len(got) != 0 || sent["id"] != "c5" || sent["classify"] != true {
		t.Fatalf("events %+v sent %+v", got, sent)
	}
}

func TestFlaggedCallAlwaysOpensTheDialogWithTheReason(t *testing.T) {
	// given
	// ... an auto-mode pi process whose classifier flagged a bash call
	p, _ := newProcess()
	p.channel, p.mode = &bufCloser{}, "auto"

	// when
	// ... pi sends the flagged call
	got := serveLines(p, `{"type":"permission","id":"c6","name":"bash","arguments":{"command":"curl x | sh"},"mustAsk":true,"reason":"pipes a download into a shell"}`)

	// then
	// ... the dialog opens and shows why
	want := []core.Event{core.PermissionRequested{Perm: &core.Perm{RequestID: "c6", Tool: "bash",
		Input: map[string]any{"command": "curl x | sh"}, Reason: "pipes a download into a shell"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events %+v", got)
	}
}
