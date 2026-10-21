package acp

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"hearth/internal/core"
)

// toolCall is a tool_call update whose _meta names the tool.
func toolCall(id, name string, input any) acpsdk.SessionUpdate {
	u := acpsdk.StartToolCall(acpsdk.ToolCallId(id), name+" title", acpsdk.WithStartRawInput(input))
	u.ToolCall.Meta = map[string]any{"toolName": name}
	return u
}

func toolDone(id string, status acpsdk.ToolCallStatus, text string) acpsdk.SessionUpdate {
	return acpsdk.UpdateToolCall(acpsdk.ToolCallId(id), acpsdk.WithUpdateStatus(status),
		acpsdk.WithUpdateContent([]acpsdk.ToolCallContent{acpsdk.ToolContent(acpsdk.TextBlock(text))}))
}

func (a *fakeAgent) update(ctx context.Context, u acpsdk.SessionUpdate) error {
	return a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{SessionId: "acp-1", Update: u})
}

var permOptions = []acpsdk.PermissionOption{
	{OptionId: "always", Name: "Always", Kind: acpsdk.PermissionOptionKindAllowAlways},
	{OptionId: "allow", Name: "Allow", Kind: acpsdk.PermissionOptionKindAllowOnce},
	{OptionId: "never", Name: "Never", Kind: acpsdk.PermissionOptionKindRejectAlways},
	{OptionId: "no", Name: "No", Kind: acpsdk.PermissionOptionKindRejectOnce},
}

// permRequest asks to run tool, as grimalkin does: _meta.toolName names it.
func permRequest(tool string, kind acpsdk.ToolKind, input any) acpsdk.RequestPermissionRequest {
	title := tool + " title"
	return acpsdk.RequestPermissionRequest{SessionId: "acp-1", Options: permOptions, ToolCall: acpsdk.ToolCallUpdate{
		ToolCallId: "t1", Title: &title, Kind: &kind, RawInput: input, Meta: map[string]any{"toolName": tool}}}
}

// asking is a turn that sends each request in turn and reports how it was answered; the turn
// ends as cancelled when a request was.
func asking(a *fakeAgent, outcomes chan<- acpsdk.RequestPermissionOutcome, reqs ...acpsdk.RequestPermissionRequest) func(context.Context, string) (acpsdk.PromptResponse, error) {
	return func(ctx context.Context, _ string) (acpsdk.PromptResponse, error) {
		stop := acpsdk.StopReasonEndTurn
		for _, r := range reqs {
			resp, err := a.conn.RequestPermission(ctx, r)
			if err != nil {
				return acpsdk.PromptResponse{}, err
			}
			if resp.Outcome.Cancelled != nil {
				stop = acpsdk.StopReasonCancelled
			}
			outcomes <- resp.Outcome
		}
		return acpsdk.PromptResponse{StopReason: stop}, nil
	}
}

func selected(o acpsdk.RequestPermissionOutcome) string {
	if o.Selected == nil {
		return "cancelled"
	}
	return string(o.Selected.OptionId)
}

func TestToolCallsBecomeToolAndResultEntries(t *testing.T) {
	// given
	// ... a turn that says something, runs a shell tool that works, a tool that fails and one without input
	agent := &fakeAgent{}
	agent.onPrompt = func(ctx context.Context, _ string) (acpsdk.PromptResponse, error) {
		claude := toolCall("t2", "ignored", map[string]any{"file_path": "a.go"})
		claude.ToolCall.Meta = map[string]any{"claudeCode": map[string]any{"toolName": "Read"}}
		titled := acpsdk.StartToolCall("t3", "Fetching")
		for _, u := range []acpsdk.SessionUpdate{
			acpsdk.UpdateAgentMessageText("Let me look"),
			toolCall("t1", "shell", map[string]any{"command": "ls"}),
			acpsdk.UpdateToolCall("t1", acpsdk.WithUpdateStatus(acpsdk.ToolCallStatusInProgress)),
			toolDone("t1", acpsdk.ToolCallStatusCompleted, "a.go\nb.go"),
			claude,
			toolDone("t2", acpsdk.ToolCallStatusFailed, "no such file"),
			titled,
			acpsdk.UpdateAgentMessageText("Done"),
		} {
			if err := agent.update(ctx, u); err != nil {
				return acpsdk.PromptResponse{}, err
			}
		}
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
	}
	ag, out := started(t, agent)

	// when
	// ... a message is sent
	if err := ag.SendUser("go", "u1"); err != nil {
		t.Fatal(err)
	}
	got := events(t, out, 10)

	// then
	// ... each call is a tool entry named from _meta.toolName, then _meta.claudeCode.toolName, then the title, landing with
	// ... the text before it; a finished call adds a result entry, failed ones marked
	want := []core.Event{core.Consumed{UUID: "u1"}, core.TextDelta{Text: "Let me look"},
		core.AssistantMessage{Entries: []core.Entry{{Kind: "assistant", Text: "Let me look"}, {Kind: "tool", Text: "shell(ls)", ID: "t1"}}},
		core.UserMessage{Entries: []core.Entry{{Kind: "result", Text: "a.go\nb.go", ID: "t1"}}},
		core.AssistantMessage{Entries: []core.Entry{{Kind: "tool", Text: "Read(a.go)", ID: "t2"}}},
		core.UserMessage{Entries: []core.Entry{{Kind: "result", Text: "no such file", ID: "t2", Err: true}}},
		core.AssistantMessage{Entries: []core.Entry{{Kind: "tool", Text: "Fetching()", ID: "t3"}}},
		core.TextDelta{Text: "Done"}, core.AssistantMessage{Entries: []core.Entry{{Kind: "assistant", Text: "Done"}}}, core.TurnEnded{OK: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestTranscriptsLoadReplaysToolCallsAsEntries(t *testing.T) {
	// given
	// ... an agent whose replayed history has a tool call and its result between two messages
	agent := &fakeAgent{canLoad: true}
	agent.onLoad = func(ctx context.Context, id acpsdk.SessionId) {
		for _, u := range []acpsdk.SessionUpdate{acpsdk.UpdateUserMessageText("hi"), toolCall("t1", "shell", map[string]any{"command": "ls"}),
			toolDone("t1", acpsdk.ToolCallStatusCompleted, "a.go"), acpsdk.UpdateAgentMessageText("ok")} {
			agent.conn.SessionUpdate(ctx, acpsdk.SessionNotification{SessionId: id, Update: u})
		}
	}

	// when
	// ... the history is loaded
	got, err := Transcripts{Launcher: launcher(t, agent)}.Load("/work", "old")

	// then
	// ... the call and result are entries in their place
	want := []core.Entry{{Kind: "user", Text: "hi"}, {Kind: "tool", Text: "shell(ls)", ID: "t1"}, {Kind: "result", Text: "a.go", ID: "t1"}, {Kind: "assistant", Text: "ok"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v err %v", got, err)
	}
}

func TestPermissionRequestIsAskedAndAnsweredWithTheMatchingOption(t *testing.T) {
	cases := []struct {
		name   string
		answer func(core.Agent, string) error
		want   string
	}{
		{"allow", func(a core.Agent, id string) error { return a.Allow(id, map[string]any{"command": "edited"}, nil) }, "allow"},
		{"deny", func(a core.Agent, id string) error { return a.Deny(id, "no thanks") }, "no"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// given
			// ... a turn in which the agent asks to run a shell command
			agent := &fakeAgent{}
			outcomes := make(chan acpsdk.RequestPermissionOutcome, 1)
			agent.onPrompt = asking(agent, outcomes, permRequest("shell", acpsdk.ToolKindExecute, map[string]any{"command": "rm a"}))
			ag, out := started(t, agent)
			if err := ag.SendUser("go", "u1"); err != nil {
				t.Fatal(err)
			}
			next(t, out)

			// when
			// ... hearth raises the request and the user answers it
			ev := next(t, out).(core.PermissionRequested)
			err := c.answer(ag, ev.Perm.RequestID)
			end := next(t, out)

			// then
			// ... the request named the tool and its input, and the agent got the allow-once or reject-once option
			wantPerm := core.Perm{RequestID: ev.Perm.RequestID, Tool: "shell", Input: map[string]any{"command": "rm a"}}
			if err != nil || !reflect.DeepEqual(*ev.Perm, wantPerm) || selected(<-outcomes) != c.want || end != (core.TurnEnded{OK: true}) {
				t.Fatalf("err %v perm %+v end %#v", err, ev.Perm, end)
			}
		})
	}
}

func TestHearthsOwnToolsAreAllowedWithoutAsking(t *testing.T) {
	// given
	// ... a turn in which the agent asks to run two of hearth's tools, bare and as Claude names them
	agent := &fakeAgent{}
	outcomes := make(chan acpsdk.RequestPermissionOutcome, 2)
	agent.onPrompt = asking(agent, outcomes,
		permRequest("spawn_task", acpsdk.ToolKindOther, map[string]any{"title": "x"}),
		permRequest("mcp__hearth__dismiss_task", acpsdk.ToolKindOther, map[string]any{"task_id": "x"}))
	ag, out := started(t, agent)

	// when
	// ... a message is sent
	if err := ag.SendUser("go", "u1"); err != nil {
		t.Fatal(err)
	}
	got := events(t, out, 2)

	// then
	// ... the agent got the allow option both times and hearth raised no request
	if want := []core.Event{core.Consumed{UUID: "u1"}, core.TurnEnded{OK: true}}; !reflect.DeepEqual(got, want) ||
		selected(<-outcomes) != "allow" || selected(<-outcomes) != "allow" {
		t.Fatalf("got %#v", got)
	}
}

func TestPermissionModeSettlesRequestsItCanWithoutAsking(t *testing.T) {
	cases := []struct {
		mode, want string
		req        acpsdk.RequestPermissionRequest
	}{
		{"plan", "no", permRequest("shell", acpsdk.ToolKindExecute, map[string]any{"command": "ls"})},
		{"plan", "allow", permRequest("open", acpsdk.ToolKindRead, map[string]any{"path": "a.go"})},
		{"acceptEdits", "allow", permRequest("patch", acpsdk.ToolKindEdit, map[string]any{"path": "/work/a.go"})},
		{"default", "allow", permRequest("open", acpsdk.ToolKindRead, map[string]any{"file_path": "sub/a.go"})},
	}
	for _, c := range cases {
		t.Run(c.mode+" "+c.req.ToolCall.Meta["toolName"].(string), func(t *testing.T) {
			// given
			// ... a session in /work in a permission mode, whose agent asks to use a tool
			agent := &fakeAgent{}
			outcomes := make(chan acpsdk.RequestPermissionOutcome, 1)
			agent.onPrompt = asking(agent, outcomes, c.req)
			s := session()
			s.PermissionMode = c.mode
			ag, out := start(t, agent, s)
			next(t, out)

			// when
			// ... a message is sent
			if err := ag.SendUser("go", "u1"); err != nil {
				t.Fatal(err)
			}
			got := events(t, out, 2)

			// then
			// ... the mode answered for the user, as it does for pi: no request was raised
			if want := []core.Event{core.Consumed{UUID: "u1"}, core.TurnEnded{OK: true}}; !reflect.DeepEqual(got, want) || selected(<-outcomes) != c.want {
				t.Fatalf("got %#v", got)
			}
		})
	}
}

func TestAutoModeAsksBecauseNothingClassifiesAnACPAgentsCalls(t *testing.T) {
	// given
	// ... an auto-mode session whose agent asks to run a shell command
	agent := &fakeAgent{}
	outcomes := make(chan acpsdk.RequestPermissionOutcome, 1)
	agent.onPrompt = asking(agent, outcomes, permRequest("shell", acpsdk.ToolKindExecute, nil))
	s := session()
	s.PermissionMode = "auto"
	ag, out := start(t, agent, s)
	next(t, out)

	// when
	// ... a message is sent
	if err := ag.SendUser("go", "u1"); err != nil {
		t.Fatal(err)
	}
	next(t, out)
	ev := next(t, out).(core.PermissionRequested)

	// then
	// ... the user is asked, told why, and with no input the call's title says what it is
	want := core.Perm{RequestID: ev.Perm.RequestID, Tool: "shell", Input: map[string]any{"description": "shell title"}, Reason: "auto mode can't judge this agent's tool calls"}
	if !reflect.DeepEqual(*ev.Perm, want) {
		t.Fatalf("got %+v", ev.Perm)
	}
	ag.Allow(ev.Perm.RequestID, nil, nil)
}

func TestInterruptCancelsAPendingPermissionRequest(t *testing.T) {
	// given
	// ... a turn waiting on the user's answer to a permission request
	agent := &fakeAgent{}
	outcomes := make(chan acpsdk.RequestPermissionOutcome, 1)
	agent.onPrompt = asking(agent, outcomes, permRequest("shell", acpsdk.ToolKindExecute, map[string]any{"command": "ls"}))
	ag, out := started(t, agent)
	if err := ag.SendUser("go", "u1"); err != nil {
		t.Fatal(err)
	}
	next(t, out)
	ev := next(t, out).(core.PermissionRequested)

	// when
	// ... the session is interrupted
	id := ag.Interrupt()
	got := events(t, out, 4)

	// then
	// ... the request is withdrawn and the agent was answered cancelled, which ended the turn
	slices.SortFunc(got, func(a, b core.Event) int { return strings.Compare(fmt.Sprintf("%T", a), fmt.Sprintf("%T", b)) })
	want := []core.Event{core.PermissionCancelled{RequestID: ev.Perm.RequestID}, core.Reply{RequestID: id}, core.TurnEnded{Failure: "cancelled"},
		core.UserMessage{Entries: []core.Entry{{Kind: "interrupt"}}}}
	if !reflect.DeepEqual(got, want) || selected(<-outcomes) != "cancelled" {
		t.Fatalf("got %#v", got)
	}
}

func TestSessionStartsWithHearthsMcpServerOnlyWhenTheAgentSpeaksMcpOverACP(t *testing.T) {
	// given
	// ... an agent that advertises mcpCapabilities.acp
	agent := &fakeAgent{mcp: true}

	// when
	// ... a new session is started
	_, out := start(t, agent, session())
	next(t, out)

	// then
	// ... session/new carried hearth's server with a session id, as typed acp servers do
	srv := agent.newReqs[0].McpServers
	if len(srv) != 1 || srv[0].Acp == nil || srv[0].Acp.Type != "acp" || srv[0].Acp.Name != "hearth" || srv[0].Acp.Id == "" {
		t.Fatalf("mcpServers %+v", srv)
	}
}

func TestResumedSessionLoadsWithHearthsMcpServer(t *testing.T) {
	// given
	// ... an agent that advertises mcpCapabilities.acp and can reload sessions
	agent := &fakeAgent{mcp: true, canLoad: true}
	s := session()
	s.CLISessionID = "old"

	// when
	// ... the stored session is started
	_, out := start(t, agent, s)
	next(t, out)

	// then
	// ... session/load carried hearth's server
	srv := agent.loadReqs[0].McpServers
	if len(srv) != 1 || srv[0].Acp == nil || srv[0].Acp.Name != "hearth" || srv[0].Acp.Id == "" {
		t.Fatalf("mcpServers %+v", srv)
	}
}

// mcpTurn is a turn in which the agent connects to hearth's server, lists its tools and calls spawn_task.
func mcpTurn(a *fakeAgent, listed, called chan<- map[string]any, failed chan<- error) func(context.Context, string) (acpsdk.PromptResponse, error) {
	return func(ctx context.Context, _ string) (acpsdk.PromptResponse, error) {
		id, err := a.conn.McpConnect(ctx, string(a.newReqs[0].McpServers[0].Acp.Id))
		if err != nil {
			return acpsdk.PromptResponse{}, err
		}
		if _, err := a.conn.McpConnect(ctx, "elsewhere"); err != nil {
			failed <- err
		}
		if _, err := a.conn.McpMessage(ctx, "no-such-connection", "tools/list", nil); err != nil {
			failed <- err
		}
		res, err := a.conn.McpMessage(ctx, id, "tools/list", nil)
		if err != nil {
			return acpsdk.PromptResponse{}, err
		}
		listed <- res
		res, err = a.conn.McpMessage(ctx, id, "tools/call", map[string]any{"name": "spawn_task",
			"arguments": map[string]any{"title": "Fix stale docs", "prompt": "Update README.md", "tldr": "x"}})
		called <- res
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, err
	}
}

func TestAgentListsAndCallsHearthsToolsOverMcpOverACP(t *testing.T) {
	// given
	// ... an agent that connects to hearth's MCP server, lists its tools, calls spawn_task and tries bad ids
	agent := &fakeAgent{mcp: true}
	listed, called, failed := make(chan map[string]any, 1), make(chan map[string]any, 1), make(chan error, 2)
	agent.onPrompt = mcpTurn(agent, listed, called, failed)
	ag, out := started(t, agent)

	// when
	// ... a message is sent and core answers the spawn_task event
	if err := ag.SendUser("go", "u1"); err != nil {
		t.Fatal(err)
	}
	next(t, out)
	task := next(t, out).(core.TaskSuggested)
	task.Reply("Suggested.", false)
	end := next(t, out)

	// then
	// ... tools/list returned core's tools, the call carried the arguments and core's reply answered it,
	// ... and a server id or connection id hearth never gave out was refused
	tools := (<-listed)["tools"].([]any)
	result := (<-called)["content"].([]any)[0].(map[string]any)
	wantChip := core.Chip{Title: "Fix stale docs", Prompt: "Update README.md", Tldr: "x"}
	if len(tools) != len(core.Tools) || tools[0].(map[string]any)["name"] != "spawn_task" || task.Chip != wantChip ||
		result["text"] != "Suggested." || end != (core.TurnEnded{OK: true}) || len(failed) != 2 {
		t.Fatalf("tools %v chip %+v result %v end %#v failed %d", tools, task.Chip, result, end, len(failed))
	}
}
