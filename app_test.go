package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

type bufCloser struct{ bytes.Buffer }

func (b *bufCloser) Close() error { return nil }

func testApp(t *testing.T) *App {
	t.Helper()
	a, err := newApp(Store{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func lastJSON(t *testing.T, b *bufCloser) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	return dir
}

func TestStoreRoundTrip(t *testing.T) {
	// given
	// ... a store holding one starred, archived session
	st := Store{t.TempDir()}
	s := &Session{SessionID: "local_1", Cwd: "/x", Title: "t", IsStarred: true, IsArchived: true, PermissionMode: "plan"}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}

	// when
	// ... loaded back and then deleted
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	delErr := st.Delete("local_1")
	after, _ := st.Load()

	// then
	// ... fields survive the round trip and deletion empties the store
	if len(got) != 1 || *got[0] != *s || delErr != nil || len(after) != 0 {
		t.Fatalf("got %+v, delete err %v, after %d", got, delErr, len(after))
	}
}

func TestLoadTranscriptBuildsEntriesFromCLIJSONL(t *testing.T) {
	// given
	// ... a CLI transcript with a user turn, a tool call, its result, a sidechain line and a reply
	path := filepath.Join(t.TempDir(), "s.jsonl")
	jsonl := `{"type":"queue-operation"}
{"type":"user","uuid":"u1","isSidechain":false,"message":{"role":"user","content":"hello"}}
{"type":"assistant","uuid":"a1","message":{"content":[{"type":"thinking","thinking":""},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","uuid":"u2","message":{"content":[{"type":"tool_result","content":"a\nb"}]}}
{"type":"assistant","uuid":"x","isSidechain":true,"message":{"content":[{"type":"text","text":"subagent"}]}}
{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"done"}]}}
`
	if err := os.WriteFile(path, []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}

	// when
	// ... the transcript is loaded
	es, err := loadTranscript(path)

	// then
	// ... entries follow the conversation, the user turn keeps its rewind UUID, sidechain is skipped
	want := []Entry{{Kind: "user", Text: "hello", UUID: "u1"}, {Kind: "tool", Text: "Bash(ls)"}, {Kind: "result", Text: "a\nb"}, {Kind: "assistant", Text: "done"}}
	if err != nil || len(es) != len(want) {
		t.Fatalf("err %v entries %+v", err, es)
	}
	for i := range want {
		if es[i] != want[i] {
			t.Fatalf("entry %d: got %+v want %+v", i, es[i], want[i])
		}
	}
}

func TestAlwaysAllowAnswersWithSuggestedPermissions(t *testing.T) {
	// given
	// ... a live session whose CLI asks to use Write with an acceptEdits suggestion
	a := testApp(t)
	s := a.newSession("/tmp", false)
	w := &bufCloser{}
	a.L(s.SessionID).agent = &Agent{w: w}
	a.handle(agentMsg{SessionID: s.SessionID, Msg: map[string]any{"type": "control_request", "request_id": "r1", "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": "Write", "input": map[string]any{"file_path": "a.txt"},
		"permission_suggestions": []any{map[string]any{"type": "setMode", "mode": "acceptEdits", "destination": "session"}}}}})
	waiting := a.status(s)

	// when
	// ... the user chooses "always"
	err := a.answerPerm(s, true, true)

	// then
	// ... the CLI gets an allow carrying the input and the suggested permission update
	resp := lastJSON(t, w)["response"].(map[string]any)
	inner := resp["response"].(map[string]any)
	if err != nil || waiting != "waiting" || resp["request_id"] != "r1" || inner["behavior"] != "allow" ||
		inner["updatedInput"].(map[string]any)["file_path"] != "a.txt" || len(inner["updatedPermissions"].([]any)) != 1 || a.status(s) != "idle" {
		t.Fatalf("err %v waiting %q resp %+v", err, waiting, resp)
	}
}

func TestAcceptedTaskSuggestionStartsSessionInWorktree(t *testing.T) {
	// given
	// ... a session in a git repo whose model called spawn_task, and a fake CLI that records stdin
	repo := gitRepo(t)
	out := filepath.Join(t.TempDir(), "stdin.jsonl")
	fake := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(fake, []byte("#!/bin/sh\ncat >> \""+out+"\"\n"), 0o755)
	t.Setenv("CLAUDE_TUI_CLI", fake)
	a := testApp(t)
	parent := a.newSession(repo, false)
	w := &bufCloser{}
	a.L(parent.SessionID).agent = &Agent{w: w}
	a.handle(agentMsg{SessionID: parent.SessionID, Msg: map[string]any{"type": "control_request", "request_id": "m1", "request": map[string]any{
		"subtype": "mcp_message", "server_name": mcpServerName, "message": map[string]any{"jsonrpc": "2.0", "id": 3.0, "method": "tools/call",
			"params": map[string]any{"name": "spawn_task", "arguments": map[string]any{"title": "Fix stale docs", "prompt": "Update README.md", "tldr": "x"}}}}}})
	mcpResp := lastJSON(t, w)["response"].(map[string]any)["response"].(map[string]any)["mcp_response"].(map[string]any)

	// when
	// ... the user accepts the chip
	s, err := a.acceptChip(parent, 0)
	defer a.closeAll()

	// then
	// ... a new linked session runs in a fresh claude/ worktree and its CLI receives the task prompt
	if err != nil || mcpResp["id"] != 3.0 || mcpResp["result"] == nil {
		t.Fatalf("err %v mcp %+v", err, mcpResp)
	}
	if !strings.HasPrefix(s.WorktreePath, filepath.Join(repo, ".claude", "worktrees", "update-readme-md-")) ||
		!strings.HasPrefix(s.Branch, "claude/update-readme-md-") || s.SourceBranch != "main" || s.Cwd != s.WorktreePath ||
		s.SpawnedFrom.SessionID != parent.SessionID || s.Title != "Fix stale docs" || len(a.L(parent.SessionID).chips) != 0 {
		t.Fatalf("session %+v", s)
	}
	deadline := time.Now().Add(5 * time.Second)
	for b, _ := os.ReadFile(out); !strings.Contains(string(b), "Update README.md"); b, _ = os.ReadFile(out) {
		if time.Now().After(deadline) {
			t.Fatalf("fake CLI stdin: %s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDeleteRefusesToDropDirtyWorktree(t *testing.T) {
	// given
	// ... a session whose worktree has an uncommitted file
	repo := gitRepo(t)
	a := testApp(t)
	s := a.newSession(repo, false)
	if err := createWorktree(s, "dirty work"); err != nil {
		t.Fatal(err)
	}
	a.save(s)
	os.WriteFile(filepath.Join(s.WorktreePath, "wip.txt"), []byte("unsaved"), 0o600)

	// when
	// ... the session is deleted
	err := a.deleteSession(s)

	// then
	// ... deletion fails, and the session record and the uncommitted file both remain
	_, statErr := os.Stat(filepath.Join(s.WorktreePath, "wip.txt"))
	left, _ := a.store.Load()
	if err == nil || statErr != nil || len(left) != 1 || a.find(s.SessionID) == nil {
		t.Fatalf("err %v stat %v left %d", err, statErr, len(left))
	}
}

func TestQueuedMessageJoinsTranscriptWhenConsumedOrComesBackWhenCancelled(t *testing.T) {
	// given
	// ... a session mid-turn with two messages sent while Claude is working
	a := testApp(t)
	s := a.newSession("/tmp", false)
	w := &bufCloser{}
	a.L(s.SessionID).agent = &Agent{w: w}
	a.send(s, "also do X")
	a.send(s, "and Y")
	l := a.L(s.SessionID)
	first, second := l.queued[0], l.queued[1]
	var pulled string
	a.unqueue(s, func(text string) { pulled = text })
	cancelReq := lastJSON(t, w)

	// when
	// ... the CLI consumes the first and confirms cancelling the second
	a.handle(agentMsg{SessionID: s.SessionID, Msg: map[string]any{"type": "user", "isReplay": true, "uuid": first.UUID,
		"message": map[string]any{"role": "user", "content": first.Text}}})
	a.handle(agentMsg{SessionID: s.SessionID, Msg: map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": cancelReq["request_id"], "response": map[string]any{"cancelled": true}}}})

	// then
	// ... the first is a rewindable transcript turn, the second is back in the user's hands, nothing stays queued
	req := cancelReq["request"].(map[string]any)
	if req["subtype"] != "cancel_async_message" || req["message_uuid"] != second.UUID {
		t.Fatalf("cancel request %+v", req)
	}
	if len(l.queued) != 0 || pulled != "and Y" || len(rewindTargets(l)) != 1 || rewindTargets(l)[0].UUID != first.UUID {
		t.Fatalf("queued %+v pulled %q entries %+v", l.queued, pulled, l.entries)
	}
}

func TestSidebarGroupsSessionsUnderProjects(t *testing.T) {
	// given
	// ... sessions from two projects in recency order, with project b collapsed
	vs := []*Session{{SessionID: "local_1", OriginCwd: "/a"}, {SessionID: "local_2", OriginCwd: "/b"}, {SessionID: "local_3", OriginCwd: "/a"}}

	// when
	// ... sidebar rows are built
	rows := groupRows(vs, map[string]bool{"/b": true})

	// then
	// ... project a leads with both its sessions, project b shows only its header
	var got []string
	for _, r := range rows {
		got = append(got, r.key())
	}
	if strings.Join(got, ",") != "p:/a,local_1,local_3,p:/b" {
		t.Fatalf("rows %v", got)
	}
}

func TestInlineMarkdownWrapsUnderItsMarker(t *testing.T) {
	// given
	// ... a reply with a heading, inline code and bold text, in a 14-column pane
	text := "# Title\nsee `go.mod` for **deps** ok"

	// when
	// ... it is styled and wrapped behind the ⏺ marker
	rows := wrap(md(text, uv.Style{}), 14, span{"⏺ ", uv.Style{}}, nil)

	// then
	// ... markers are stripped, rows break at spaces under a hanging indent, code and bold keep their styles
	var got []string
	styles := map[string]uv.Style{}
	for _, r := range rows {
		var b strings.Builder
		for _, s := range r.spans {
			b.WriteString(s.t)
			styles[s.t] = s.st
		}
		got = append(got, b.String())
	}
	if strings.Join(got, "|") != "⏺ Title|  see go.mod |  for deps ok" {
		t.Fatalf("rows %q", got)
	}
	if styles["go.mod"].Fg != th.permission || styles["deps"].Attrs != uv.AttrBold || styles["Title"].Attrs != uv.AttrBold|uv.AttrItalic {
		t.Fatalf("styles %+v", styles)
	}
}

func TestTranscriptYankRejoinsWrappedRowsWithoutMarkers(t *testing.T) {
	// given
	// ... a reply wrapped behind its ⏺ marker, one row ending in the space it broke at, then a second paragraph
	lines := screenLines(wrap(md("alpha bet gamma\nnext", uv.Style{}), 12, span{"⏺ ", uv.Style{}}, nil))
	v := &transcriptView{}
	var register []rune

	// when
	// ... all three rows are yanked linewise in the transcript view
	press(v, lines, 10, &register, "Vjjy")

	// then
	// ... the wrap adds no extra space, the paragraph break is a newline, and the marker is left out
	if len(lines) != 3 || string(register) != "alpha bet gamma\nnext" {
		t.Fatalf("rows %d register %q", len(lines), string(register))
	}
}

func TestRenderedReplyYanksAsRawMarkdown(t *testing.T) {
	// given
	// ... a reply rendered by glamour, so its rows show no markdown markup
	src := "# Title\n\nSome **bold** and `code` text.\n\n- a\n- b\n\nEnd."
	rows := markdown(src, 40, false)
	lines := screenLines(rows)
	v := &transcriptView{}
	var register []rune

	// when
	// ... the rows from the paragraph through the list are yanked linewise
	press(v, lines, 20, &register, "jjVjjjy")

	// then
	// ... the register holds those blocks' raw markdown, not the rendering
	var shown strings.Builder
	for _, sp := range rows[2].spans {
		shown.WriteString(sp.t)
	}
	if s := shown.String(); s != "  Some bold and \u00a0code\u00a0 text." {
		t.Fatalf("rendered row shows markup: %q", s)
	}
	if string(register) != "Some **bold** and `code` text.\n\n- a\n- b" {
		t.Fatalf("register %q", string(register))
	}
}

func TestVisualModeSelectsRawMarkdownWhereTheCursorWas(t *testing.T) {
	// given
	// ... a reply whose code fences make the raw layout taller, the cursor on the rendered "b" of the last item
	src := "```\nx := 1\n```\n\n- a\n- **b** c"
	rendered, raw := screenLines(markdown(src, 40, false)), screenLines(markdown(src, 40, true))
	v := &transcriptView{cursor: position{line: 3, col: 4}}
	var register []rune

	// when
	// ... the view switches to raw markdown and the rest of the line is selected and yanked
	v.cursor = remap(v.cursor, rendered, raw)
	press(v, raw, 20, &register, "v$y")

	// then
	// ... the cursor stayed on that item, now at its raw markup, and the yank holds exactly the raw characters
	if string(register) != "**b** c" {
		t.Fatalf("register %q", string(register))
	}
}
