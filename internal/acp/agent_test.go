package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"hearth/internal/core"
)

// agentConn is an agent's connection to hearth. It is a bare Connection because the SDK's agent side
// can't send mcp/message.
type agentConn struct{ *acpsdk.Connection }

func (c agentConn) SessionUpdate(ctx context.Context, n acpsdk.SessionNotification) error {
	return c.SendNotification(ctx, acpsdk.ClientMethodSessionUpdate, n)
}

func (c agentConn) RequestPermission(ctx context.Context, r acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
	return acpsdk.SendRequest[acpsdk.RequestPermissionResponse](c.Connection, ctx, acpsdk.ClientMethodSessionRequestPermission, r)
}

func (c agentConn) McpConnect(ctx context.Context, acpID string) (acpsdk.UnstableMcpConnectionId, error) {
	r, err := acpsdk.SendRequest[acpsdk.UnstableConnectMcpResponse](c.Connection, ctx, acpsdk.ClientMethodMcpConnect, acpsdk.UnstableConnectMcpRequest{AcpId: acpsdk.UnstableMcpServerAcpId(acpID)})
	return r.ConnectionId, err
}

func (c agentConn) McpMessage(ctx context.Context, id acpsdk.UnstableMcpConnectionId, method string, params map[string]any) (map[string]any, error) {
	return acpsdk.SendRequest[map[string]any](c.Connection, ctx, acpsdk.ClientMethodMcpMessage, acpsdk.UnstableMessageMcpRequest{ConnectionId: id, Method: method, Params: params})
}

// fakeAgent is an ACP agent on the agent side of a connection. onPrompt scripts each turn.
type fakeAgent struct {
	conn      agentConn
	mcp       bool // advertises mcpCapabilities.acp
	onPrompt  func(ctx context.Context, text string) (acpsdk.PromptResponse, error)
	newErr    error
	canLoad   bool                                           // advertises loadSession
	loadErr   error                                          // session/load fails with this
	onLoad    func(ctx context.Context, id acpsdk.SessionId) // streams the history session/load replays
	cancelled chan struct{}                                  // closed by session/cancel
	exit      func()                                         // the agent process ends

	mu       sync.Mutex
	newReqs  []acpsdk.NewSessionRequest
	loadReqs []acpsdk.LoadSessionRequest
	log      []string // "prompt:<text>" and "end:<text>", in order
}

func (a *fakeAgent) Initialize(context.Context, acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	return acpsdk.InitializeResponse{ProtocolVersion: acpsdk.ProtocolVersionNumber, AgentInfo: &acpsdk.Implementation{Name: "fake", Version: "9.9"},
		AgentCapabilities: acpsdk.AgentCapabilities{LoadSession: a.canLoad, McpCapabilities: acpsdk.McpCapabilities{Acp: a.mcp}}}, nil
}

func (a *fakeAgent) handle(ctx context.Context, method string, raw json.RawMessage) (any, *acpsdk.RequestError) {
	switch method {
	case acpsdk.AgentMethodInitialize:
		return serve(raw, func(r acpsdk.InitializeRequest) (any, error) { return a.Initialize(ctx, r) })
	case acpsdk.AgentMethodSessionNew:
		return serve(raw, func(r acpsdk.NewSessionRequest) (any, error) { return a.NewSession(ctx, r) })
	case acpsdk.AgentMethodSessionLoad:
		return serve(raw, func(r acpsdk.LoadSessionRequest) (any, error) { return a.LoadSession(ctx, r) })
	case acpsdk.AgentMethodSessionPrompt:
		return serve(raw, func(r acpsdk.PromptRequest) (any, error) { return a.Prompt(ctx, r) })
	case acpsdk.AgentMethodSessionCancel:
		return serve(raw, func(r acpsdk.CancelNotification) (any, error) { return nil, a.Cancel(ctx, r) })
	}
	return nil, acpsdk.NewMethodNotFound(method)
}

func (a *fakeAgent) LoadSession(ctx context.Context, r acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error) {
	a.mu.Lock()
	a.loadReqs = append(a.loadReqs, r)
	a.mu.Unlock()
	if a.loadErr != nil {
		return acpsdk.LoadSessionResponse{}, a.loadErr
	}
	if a.onLoad != nil {
		a.onLoad(ctx, r.SessionId)
	}
	return acpsdk.LoadSessionResponse{}, nil
}

// replayed streams a history over session/update: "u:" chunks are the user's, the rest the agent's.
func (a *fakeAgent) replayed(chunks ...string) func(context.Context, acpsdk.SessionId) {
	return func(ctx context.Context, id acpsdk.SessionId) {
		for _, c := range chunks {
			u := acpsdk.UpdateAgentMessageText(c)
			if t, ok := strings.CutPrefix(c, "u:"); ok {
				u = acpsdk.UpdateUserMessageText(t)
			}
			a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{SessionId: id, Update: u})
		}
	}
}

func (a *fakeAgent) NewSession(_ context.Context, r acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.newReqs = append(a.newReqs, r)
	if a.newErr != nil {
		return acpsdk.NewSessionResponse{}, a.newErr
	}
	return acpsdk.NewSessionResponse{SessionId: acpsdk.SessionId(fmt.Sprintf("acp-%d", len(a.newReqs)))}, nil
}

func (a *fakeAgent) Prompt(ctx context.Context, r acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	text := r.Prompt[0].Text.Text
	a.record("prompt:" + text)
	defer a.record("end:" + text)
	return a.onPrompt(ctx, text)
}

func (a *fakeAgent) record(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.log = append(a.log, s)
}

func (a *fakeAgent) Cancel(context.Context, acpsdk.CancelNotification) error {
	close(a.cancelled)
	return nil
}

func (a *fakeAgent) say(ctx context.Context, chunks ...string) error {
	for _, c := range chunks {
		if err := a.conn.SessionUpdate(ctx, acpsdk.SessionNotification{SessionId: "acp-1", Update: acpsdk.UpdateAgentMessageText(c)}); err != nil {
			return err
		}
	}
	return nil
}

// replies is a turn that streams chunks and ends.
func replies(a *fakeAgent, chunks ...string) func(context.Context, string) (acpsdk.PromptResponse, error) {
	return func(ctx context.Context, _ string) (acpsdk.PromptResponse, error) {
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, a.say(ctx, chunks...)
	}
}

// launcher is a Launcher whose process is in-memory pipes to agent.
func launcher(t *testing.T, agent *fakeAgent) Launcher {
	t.Helper()
	toAgentR, toAgentW := io.Pipe()
	fromAgentR, fromAgentW := io.Pipe()
	agent.cancelled = make(chan struct{})
	agent.conn = agentConn{acpsdk.NewConnection(agent.handle, fromAgentW, toAgentR)}
	agent.exit = func() { fromAgentW.Close(); toAgentR.Close() }
	t.Cleanup(agent.exit)
	return Launcher{spawn: func(string) (transport, error) {
		return transport{in: toAgentW, out: fromAgentR, wait: func() error { return nil }, kill: agent.exit}, nil
	}}
}

// start launches a Launcher over in-memory pipes to agent and returns the Agent and its events.
func start(t *testing.T, agent *fakeAgent, s *core.Session) (core.Agent, chan core.AgentEvent) {
	t.Helper()
	l := launcher(t, agent)
	out := make(chan core.AgentEvent, 64)
	ag, err := l.Start(s, core.Tools, out)
	if err != nil {
		t.Fatal(err)
	}
	return ag, out
}

func session() *core.Session { return &core.Session{SessionID: "h1", Cwd: "/work"} }

func next(t *testing.T, out <-chan core.AgentEvent) core.Event {
	t.Helper()
	select {
	case m := <-out:
		if m.SessionID != "h1" {
			t.Fatalf("event for session %q", m.SessionID)
		}
		if e, ok := m.Event.(core.TurnEnded); ok {
			e.Duration = 0
			return e
		}
		return m.Event
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return nil
	}
}

// events reads n events.
func events(t *testing.T, out <-chan core.AgentEvent, n int) []core.Event {
	t.Helper()
	var got []core.Event
	for range n {
		got = append(got, next(t, out))
	}
	return got
}

// started is start, past the Initialized event.
func started(t *testing.T, agent *fakeAgent) (core.Agent, chan core.AgentEvent) {
	t.Helper()
	ag, out := start(t, agent, session())
	next(t, out)
	return ag, out
}

func TestStartInitializesAndReportsTheAgentsSession(t *testing.T) {
	// given
	// ... an ACP agent that names its session acp-1
	agent := &fakeAgent{}

	// when
	// ... hearth starts a session in /work
	_, out := start(t, agent, session())
	got := next(t, out)

	// then
	// ... Initialized carries the agent's version and session id, and session/new had the cwd, no MCP servers and no _meta
	want := core.Initialized{Version: "9.9", CLISessionID: "acp-1"}
	req := agent.newReqs[0]
	if got != want || req.Cwd != "/work" || req.McpServers == nil || len(req.McpServers) != 0 || req.Meta != nil {
		t.Fatalf("got %#v new session %+v", got, req)
	}
}

func TestRelaunchStartsFreshAndSaysHistoryWasNotRestored(t *testing.T) {
	// given
	// ... a session that ran before, with the agent's old session id
	agent := &fakeAgent{}
	s := session()
	s.CLISessionID = "old"

	// when
	// ... it is started again
	_, out := start(t, agent, s)
	got := events(t, out, 2)

	// then
	// ... a new agent session replaces the old id, and an info entry says the history was not restored
	want := []core.Event{core.Initialized{Version: "9.9", CLISessionID: "acp-1"},
		core.UserMessage{Entries: []core.Entry{{Kind: "info", Text: "history was not restored: this agent can't reload a session yet, so this is a fresh conversation"}}}}
	if !reflect.DeepEqual(got, want) || len(agent.loadReqs) != 0 {
		t.Fatalf("got %#v session/load sent %d times", got, len(agent.loadReqs))
	}
}

func TestRelaunchLoadsTheStoredSessionWithoutReemittingTheReplay(t *testing.T) {
	// given
	// ... an agent that can reload sessions and replays a two-message history
	agent := &fakeAgent{canLoad: true}
	agent.onLoad = agent.replayed("u:hi", "Hel", "lo")
	agent.onPrompt = replies(agent, "ok")
	s := session()
	s.CLISessionID = "old"

	// when
	// ... the session is started again and then sent a message
	ag, out := start(t, agent, s)
	got := next(t, out)
	if err := ag.SendUser("more", "u1"); err != nil {
		t.Fatal(err)
	}
	after := next(t, out)

	// then
	// ... session/load carried the stored id, cwd and no MCP servers, session/new was not sent,
	// ... Initialized names the same id, and the first event after it is the new message, not the replay
	req := agent.loadReqs[0]
	wantReq := acpsdk.LoadSessionRequest{SessionId: "old", Cwd: "/work", McpServers: []acpsdk.McpServer{}}
	if !reflect.DeepEqual(req, wantReq) || len(agent.newReqs) != 0 ||
		got != (core.Initialized{Version: "9.9", CLISessionID: "old"}) || after != (core.Consumed{UUID: "u1"}) {
		t.Fatalf("load %+v new %d got %#v then %#v", req, len(agent.newReqs), got, after)
	}
}

func TestRelaunchFallsBackToAFreshSessionWhenLoadFails(t *testing.T) {
	// given
	// ... an agent that can reload sessions but doesn't know this one
	agent := &fakeAgent{canLoad: true, loadErr: errors.New("unknown session old")}
	s := session()
	s.CLISessionID = "old"

	// when
	// ... the session is started again
	_, out := start(t, agent, s)
	got := events(t, out, 2)

	// then
	// ... a new agent session replaces the id, and an info entry carries the agent's error
	want := []core.Event{core.Initialized{Version: "9.9", CLISessionID: "acp-1"},
		core.UserMessage{Entries: []core.Entry{{Kind: "info", Text: "history was not restored: session/load: unknown session old, so this is a fresh conversation"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestTranscriptsLoadBuildsEntriesFromTheReplayAndClosesTheAgent(t *testing.T) {
	// given
	// ... an agent that replays two exchanges, each reply in chunks
	agent := &fakeAgent{canLoad: true}
	agent.onLoad = agent.replayed("u:hi", "Hel", "lo", "u:and ", "u:again", "ok")

	// when
	// ... the history of a session is loaded
	got, err := Transcripts{Launcher: launcher(t, agent)}.Load("/work", "old")

	// then
	// ... the entries are the messages in order with their chunks joined, the stored id was loaded and the agent's stdin is closed
	want := []core.Entry{{Kind: "user", Text: "hi"}, {Kind: "assistant", Text: "Hello"}, {Kind: "user", Text: "and again"}, {Kind: "assistant", Text: "ok"}}
	if err != nil || !reflect.DeepEqual(got, want) || agent.loadReqs[0].SessionId != "old" {
		t.Fatalf("got %#v err %v load %+v", got, err, agent.loadReqs)
	}
	select {
	case <-agent.conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the agent was not closed")
	}
}

func TestTranscriptsLoadFailsForAnAgentThatCannotReload(t *testing.T) {
	// given
	// ... an agent that does not advertise loadSession
	agent := &fakeAgent{}

	// when
	// ... the history of a session is loaded
	got, err := Transcripts{Launcher: launcher(t, agent)}.Load("/work", "old")

	// then
	// ... it fails without sending session/load
	if err == nil || got != nil || len(agent.loadReqs) != 0 {
		t.Fatalf("got %#v err %v", got, err)
	}
}

func TestIdleMessageIsPromptedAndItsReplyEndsTheTurn(t *testing.T) {
	// given
	// ... a started agent that answers in two chunks
	agent := &fakeAgent{}
	agent.onPrompt = replies(agent, "Hel", "lo")
	ag, out := started(t, agent)

	// when
	// ... a message is sent while idle
	if err := ag.SendUser("hi", "u1"); err != nil {
		t.Fatal(err)
	}
	got := events(t, out, 5)

	// then
	// ... it is consumed as it is sent, the chunks stream, the reply lands whole and the turn ends OK
	want := []core.Event{core.Consumed{UUID: "u1"}, core.TextDelta{Text: "Hel"}, core.TextDelta{Text: "lo"},
		core.AssistantMessage{Entries: []core.Entry{{Kind: "assistant", Text: "Hello"}}}, core.TurnEnded{OK: true}}
	if !reflect.DeepEqual(got, want) || !slices.Equal(agent.log, []string{"prompt:hi", "end:hi"}) {
		t.Fatalf("got %#v agent saw %v", got, agent.log)
	}
}

func TestMessageDuringATurnIsHeldAndSentWhenTheTurnEnds(t *testing.T) {
	// given
	// ... a turn in flight that ends only when released
	agent := &fakeAgent{}
	release := make(chan struct{})
	agent.onPrompt = func(context.Context, string) (acpsdk.PromptResponse, error) {
		<-release
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
	}
	ag, out := started(t, agent)
	if err := ag.SendUser("first", "u1"); err != nil {
		t.Fatal(err)
	}
	next(t, out)

	// when
	// ... a second message is sent mid-turn, and the turn then ends
	if err := ag.SendUser("second", "u2"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // a prompt sent at once would have reached the agent by now
	close(release)
	got := events(t, out, 3)

	// then
	// ... the first turn ends before the second message is consumed, and the agent saw the prompts one after the other
	want := []core.Event{core.TurnEnded{OK: true}, core.Consumed{UUID: "u2"}, core.TurnEnded{OK: true}}
	wantLog := []string{"prompt:first", "end:first", "prompt:second", "end:second"}
	if !reflect.DeepEqual(got, want) || !slices.Equal(agent.log, wantLog) {
		t.Fatalf("got %#v agent saw %v", got, agent.log)
	}
}

func TestInterruptCancelsTheTurnAndItEndsInterrupted(t *testing.T) {
	// given
	// ... a turn that runs until the agent is cancelled, then stops as cancelled
	agent := &fakeAgent{}
	agent.onPrompt = func(context.Context, string) (acpsdk.PromptResponse, error) {
		<-agent.cancelled
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
	}
	ag, out := started(t, agent)
	if err := ag.SendUser("go", "u1"); err != nil {
		t.Fatal(err)
	}
	next(t, out)

	// when
	// ... the session is interrupted
	id := ag.Interrupt()
	got := events(t, out, 3)

	// then
	// ... the request is answered, an interrupt entry shows and the turn ends not OK; the answer may come before or after the turn's end
	slices.SortFunc(got, func(a, b core.Event) int { return strings.Compare(fmt.Sprintf("%T", a), fmt.Sprintf("%T", b)) })
	want := []core.Event{core.Reply{RequestID: id}, core.TurnEnded{Failure: "cancelled"}, core.UserMessage{Entries: []core.Entry{{Kind: "interrupt"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestPromptErrorEndsTheTurnWithItsText(t *testing.T) {
	// given
	// ... an agent whose prompt fails
	agent := &fakeAgent{}
	agent.onPrompt = func(context.Context, string) (acpsdk.PromptResponse, error) {
		return acpsdk.PromptResponse{}, errors.New("model offline")
	}
	ag, out := started(t, agent)

	// when
	// ... a message is sent
	if err := ag.SendUser("hi", "u1"); err != nil {
		t.Fatal(err)
	}
	got := events(t, out, 2)

	// then
	// ... the turn ends not OK, with the agent's error text
	want := []core.Event{core.Consumed{UUID: "u1"}, core.TurnEnded{Failure: "model offline"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestAgentExitingReportsExited(t *testing.T) {
	// given
	// ... a started agent
	agent := &fakeAgent{}
	_, out := started(t, agent)

	// when
	// ... the agent process ends
	agent.exit()
	got := next(t, out)

	// then
	// ... Exited arrives
	if got != (core.Exited{Err: io.EOF}) {
		t.Fatalf("got %#v", got)
	}
}

func TestFailedSessionStartExitsWithTheAgentsError(t *testing.T) {
	// given
	// ... an agent that refuses session/new
	agent := &fakeAgent{newErr: errors.New("no api key")}

	// when
	// ... hearth starts a session
	_, out := start(t, agent, session())
	got := next(t, out)

	// then
	// ... the agent is stopped and Exited carries the agent's reason
	want := core.Exited{Err: errors.New("session/new: no api key")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestUnsupportedRequestsReplyWithAnError(t *testing.T) {
	cases := []struct {
		name string
		call func(core.Agent) string
		want string
	}{
		{"cancel queued", func(a core.Agent) string { return a.CancelQueued("u1") }, "this agent does not support cancelling one queued message yet"},
		{"model", func(a core.Agent) string { return a.SetModel("m") }, "this agent does not support switching models yet"},
		{"effort", func(a core.Agent) string { return a.SetEffort("high") }, "this agent does not support setting an effort yet"},
		{"rename", func(a core.Agent) string { return a.Rename("t") }, "this agent does not support renaming a session yet"},
		{"title", func(a core.Agent) string { return a.GenerateTitle("d") }, "this agent does not support generating a title yet"},
		{"rewind", func(a core.Agent) string { return a.Rewind("u1", false) }, "this agent does not support file rewind yet"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// given
			// ... a started agent
			ag, out := started(t, &fakeAgent{})

			// when
			// ... an operation it can't do is requested
			id := c.call(ag)
			got := next(t, out)

			// then
			// ... a Reply with that request id carries the error
			if want := (core.Reply{RequestID: id, Err: c.want}); got != want {
				t.Fatalf("got %#v want %#v", got, want)
			}
		})
	}
}
