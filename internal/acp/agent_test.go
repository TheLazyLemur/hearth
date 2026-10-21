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
	conn        agentConn
	mcp         bool // advertises mcpCapabilities.acp
	onPrompt    func(ctx context.Context, text string) (acpsdk.PromptResponse, error)
	newErr      error
	canLoad     bool                                           // advertises loadSession
	loadErr     error                                          // session/load fails with this
	onLoad      func(ctx context.Context, id acpsdk.SessionId) // streams the history session/load replays
	cancelled   chan struct{}                                  // closed by session/cancel
	exit        func()                                         // the agent process ends
	steering    bool                                           // advertises _meta.steering.supported
	onSteer     func(text string) (string, error)              // answers each _session/steering with an outcome
	canClose    bool                                           // advertises sessionCapabilities.close
	models      []fakeModel                                    // the model and thought_level options it exposes; none when empty
	metaEfforts bool                                           // each model choice carries its efforts in _meta.efforts
	byID        bool                                           // names no option categories, only the ids model and thought_level
	setErr      map[string]error                               // session/set_config_option fails for these values

	mu       sync.Mutex
	newReqs  []acpsdk.NewSessionRequest
	loadReqs []acpsdk.LoadSessionRequest
	steers   []steerRequest
	model    string                                 // the model it runs, the first of models until one is set
	effort   string                                 // the thought_level it is on, the model's first until one is set
	sets     []acpsdk.SetSessionConfigOptionValueId // session/set_config_option calls, in order
	closed   []acpsdk.SessionId
	log      []string // "prompt:<text>" and "end:<text>", in order
}

func (a *fakeAgent) Initialize(context.Context, acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	r := acpsdk.InitializeResponse{ProtocolVersion: acpsdk.ProtocolVersionNumber, AgentInfo: &acpsdk.Implementation{Name: "fake", Version: "9.9"},
		AgentCapabilities: acpsdk.AgentCapabilities{LoadSession: a.canLoad, McpCapabilities: acpsdk.McpCapabilities{Acp: a.mcp}}}
	if a.canClose {
		r.AgentCapabilities.SessionCapabilities.Close = &acpsdk.SessionCloseCapabilities{}
	}
	if a.steering {
		r.Meta = map[string]any{"steering": map[string]any{"supported": true}}
	}
	return r, nil
}

func (a *fakeAgent) Steer(_ context.Context, r steerRequest) (steerResponse, error) {
	text := r.Prompt[0].Text.Text
	a.mu.Lock()
	a.steers = append(a.steers, r)
	a.mu.Unlock()
	outcome, err := a.onSteer(text)
	return steerResponse{Outcome: outcome}, err
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
	case acpsdk.AgentMethodSessionSetConfigOption:
		return serve(raw, func(r acpsdk.SetSessionConfigOptionRequest) (any, error) { return a.SetConfigOption(r) })
	case acpsdk.AgentMethodSessionClose:
		return serve(raw, func(r acpsdk.CloseSessionRequest) (any, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.closed = append(a.closed, r.SessionId)
			return acpsdk.CloseSessionResponse{}, nil
		})
	case "_session/steering":
		return serve(raw, func(r steerRequest) (any, error) { return a.Steer(ctx, r) })
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
	return acpsdk.LoadSessionResponse{ConfigOptions: a.options()}, nil
}

// fakeModel is a model the fake agent offers and the thought_level values it accepts.
type fakeModel struct {
	id      string
	efforts []string
}

// options is the config options as the agent has them now. The thought_level values are the current model's.
func (a *fakeAgent) options() []acpsdk.SessionConfigOption {
	if len(a.models) == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.model == "" {
		a.model, a.effort = a.models[0].id, a.models[0].efforts[0]
	}
	var ids []string
	var efforts []string
	for _, m := range a.models {
		ids = append(ids, m.id)
		if m.id == a.model {
			efforts = m.efforts
		}
	}
	modelID, effortID := "m", "t"
	modelCat, effortCat := acpsdk.SessionConfigOptionCategoryModel, acpsdk.SessionConfigOptionCategoryThoughtLevel
	if a.byID {
		modelID, effortID = "model", "thought_level"
	}
	model := fakeSelect(modelID, a.model, ids)
	if a.metaEfforts {
		vs := *model.Select.Options.Ungrouped
		for i, m := range a.models {
			listed := []any{}
			for _, e := range m.efforts {
				listed = append(listed, e)
			}
			vs[i].Meta = map[string]any{"efforts": listed}
		}
	}
	effort := fakeSelect(effortID, a.effort, efforts)
	if !a.byID {
		model.Select.Category, effort.Select.Category = &modelCat, &effortCat
	}
	return []acpsdk.SessionConfigOption{model, effort}
}

func fakeSelect(id, current string, values []string) acpsdk.SessionConfigOption {
	var vs acpsdk.SessionConfigSelectOptionsUngrouped
	for _, v := range values {
		d := v + " description"
		vs = append(vs, acpsdk.SessionConfigSelectOption{Name: v + " name", Value: acpsdk.SessionConfigValueId(v), Description: &d})
	}
	o := acpsdk.NewSessionConfigOptionSelect(acpsdk.SessionConfigValueId(current), acpsdk.SessionConfigSelectOptions{Ungrouped: &vs})
	o.Select.Id, o.Select.Name = acpsdk.SessionConfigId(id), id
	return o
}

// SetConfigOption switches the model, which resets the effort to the new model's first, or sets the effort.
func (a *fakeAgent) SetConfigOption(r acpsdk.SetSessionConfigOptionRequest) (acpsdk.SetSessionConfigOptionResponse, error) {
	a.options() // fixes the starting state
	a.mu.Lock()
	a.sets = append(a.sets, *r.ValueId)
	err := a.setErr[string(r.ValueId.Value)]
	if err == nil {
		switch r.ValueId.ConfigId {
		case "m", "model":
			for _, m := range a.models {
				if string(r.ValueId.Value) == m.id {
					a.model, a.effort = m.id, m.efforts[0]
				}
			}
		default:
			err = fmt.Errorf("%q is not a thought_level of %s", r.ValueId.Value, a.model)
			for _, m := range a.models {
				if m.id == a.model && slices.Contains(m.efforts, string(r.ValueId.Value)) {
					a.effort, err = string(r.ValueId.Value), nil
				}
			}
		}
	}
	a.mu.Unlock()
	if err != nil {
		return acpsdk.SetSessionConfigOptionResponse{}, err
	}
	return acpsdk.SetSessionConfigOptionResponse{ConfigOptions: a.options()}, nil
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
	a.newReqs = append(a.newReqs, r)
	n := len(a.newReqs)
	a.mu.Unlock()
	if a.newErr != nil {
		return acpsdk.NewSessionResponse{}, a.newErr
	}
	return acpsdk.NewSessionResponse{SessionId: acpsdk.SessionId(fmt.Sprintf("acp-%d", n)), ConfigOptions: a.options()}, nil
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

// blockedFirst is a turn on "first" that ends only when released; other prompts end at once.
func blockedFirst(release <-chan struct{}) func(context.Context, string) (acpsdk.PromptResponse, error) {
	return func(_ context.Context, text string) (acpsdk.PromptResponse, error) {
		if text == "first" {
			<-release
		}
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn}, nil
	}
}

// steered is a started steering agent whose "first" turn runs until release is closed, with first
// already consumed. It answers each steer as onSteer says.
func steered(t *testing.T, release <-chan struct{}, onSteer func(string) (string, error)) (*fakeAgent, core.Agent, chan core.AgentEvent) {
	t.Helper()
	agent := &fakeAgent{steering: true, onSteer: onSteer, onPrompt: blockedFirst(release)}
	ag, out := started(t, agent)
	if err := ag.SendUser("first", "u1"); err != nil {
		t.Fatal(err)
	}
	next(t, out)
	return agent, ag, out
}

func always(outcome string, err error) func(string) (string, error) {
	return func(string) (string, error) { return outcome, err }
}

func TestMessageDuringATurnSteersAndIsConsumedWhenInjected(t *testing.T) {
	// given
	// ... a turn in flight on an agent that supports steering and injects
	release := make(chan struct{})
	agent, ag, out := steered(t, release, always("injected", nil))

	// when
	// ... a second message is sent mid-turn
	if err := ag.SendUser("second", "u2"); err != nil {
		t.Fatal(err)
	}
	got := next(t, out)

	// then
	// ... it is consumed before the turn ends, the agent got _session/steering with the text and the idle opt-in, and no prompt for it
	req := agent.steers[0]
	wantMeta := map[string]any{"steering": map[string]any{"idleBehavior": "promptRequired"}}
	if got != (core.Consumed{UUID: "u2"}) || req.SessionId != "acp-1" || req.Prompt[0].Text.Text != "second" || !reflect.DeepEqual(req.Meta, wantMeta) ||
		!slices.Equal(agent.log, []string{"prompt:first"}) {
		t.Fatalf("got %#v steer %+v agent saw %v", got, req, agent.log)
	}
	close(release)
	if end := next(t, out); end != (core.TurnEnded{OK: true}) {
		t.Fatalf("got %#v", end)
	}
}

func TestSteerThatCannotInjectFallsBackToTheNextPrompt(t *testing.T) {
	cases := []struct {
		name    string
		outcome string
		err     error
	}{
		{"promptRequired", "promptRequired", nil},
		{"failed", "failed", nil},
		{"error", "", errors.New("malformed")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// given
			// ... a turn in flight on a steering agent that refuses the steer
			release := make(chan struct{})
			agent, ag, out := steered(t, release, always(c.outcome, c.err))

			// when
			// ... a second message is sent mid-turn, and the turn then ends
			if err := ag.SendUser("second", "u2"); err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond) // the steer has been answered by now
			close(release)
			got := events(t, out, 3)

			// then
			// ... the message is consumed only when it is sent as the next prompt, after the first turn ends
			want := []core.Event{core.TurnEnded{OK: true}, core.Consumed{UUID: "u2"}, core.TurnEnded{OK: true}}
			wantLog := []string{"prompt:first", "end:first", "prompt:second", "end:second"}
			if !reflect.DeepEqual(got, want) || !slices.Equal(agent.log, wantLog) || len(agent.steers) != 1 {
				t.Fatalf("got %#v agent saw %v steers %d", got, agent.log, len(agent.steers))
			}
		})
	}
}

func TestSteerThatStartedATurnIsConsumedAndNotPromptedAgain(t *testing.T) {
	// given
	// ... a turn in flight on an agent that starts its own turn for a steer
	release := make(chan struct{})
	agent, ag, out := steered(t, release, always("startedNewTurn", nil))

	// when
	// ... a second message is sent mid-turn, and the turn then ends
	if err := ag.SendUser("second", "u2"); err != nil {
		t.Fatal(err)
	}
	got := []core.Event{next(t, out)}
	close(release)
	got = append(got, next(t, out))

	// then
	// ... it is consumed at once, and no prompt is ever sent for it
	want := []core.Event{core.Consumed{UUID: "u2"}, core.TurnEnded{OK: true}}
	if !reflect.DeepEqual(got, want) || !slices.Equal(agent.log, []string{"prompt:first", "end:first"}) {
		t.Fatalf("got %#v agent saw %v", got, agent.log)
	}
}

func TestMessagesAfterAFallbackAreHeldInOrder(t *testing.T) {
	// given
	// ... a turn in flight on a steering agent that refuses to steer "second" but would inject "third"
	release := make(chan struct{})
	agent, ag, out := steered(t, release, func(text string) (string, error) {
		if text == "second" {
			return "promptRequired", nil
		}
		return "injected", nil
	})

	// when
	// ... "second" then "third" are sent mid-turn, and the turn then ends
	for _, m := range []heldMessage{{"second", "u2"}, {"third", "u3"}} {
		if err := ag.SendUser(m.text, m.uuid); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond) // both have been placed by now
	close(release)
	got := events(t, out, 5)

	// then
	// ... both go out as prompts in order, "third" was never steered, so it can't overtake "second"
	want := []core.Event{core.TurnEnded{OK: true}, core.Consumed{UUID: "u2"}, core.TurnEnded{OK: true}, core.Consumed{UUID: "u3"}, core.TurnEnded{OK: true}}
	wantLog := []string{"prompt:first", "end:first", "prompt:second", "end:second", "prompt:third", "end:third"}
	if !reflect.DeepEqual(got, want) || !slices.Equal(agent.log, wantLog) || len(agent.steers) != 1 {
		t.Fatalf("got %#v agent saw %v steers %d", got, agent.log, len(agent.steers))
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

// twoModels is an agent exposing two models whose thought_level lists differ.
func twoModels() *fakeAgent {
	return &fakeAgent{models: []fakeModel{{"p/fast", []string{"low", "high"}}, {"p/deep", []string{"low", "medium", "max"}}}}
}

func set(config, value string) acpsdk.SetSessionConfigOptionValueId {
	return acpsdk.SetSessionConfigOptionValueId{ConfigId: acpsdk.SessionConfigId(config), Value: acpsdk.SessionConfigValueId(value), SessionId: "acp-1"}
}

func TestModelsListsEachModelWithItsEffortsAndClosesTheSession(t *testing.T) {
	// given
	// ... an agent with two models whose thought_level lists differ, which can close sessions
	agent := twoModels()
	agent.canClose = true

	// when
	// ... the launcher lists its models
	got, err := launcher(t, agent).Models("/work")

	// then
	// ... each model carries its name, description and own efforts, and the throwaway session was closed
	want := []core.Model{
		{ID: "p/fast", Name: "p/fast name", Description: "p/fast description", Efforts: []string{"low", "high"}},
		{ID: "p/deep", Name: "p/deep name", Description: "p/deep description", Efforts: []string{"low", "medium", "max"}},
	}
	if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(agent.closed, []acpsdk.SessionId{"acp-1"}) {
		t.Fatalf("got %#v err %v closed %v", got, err, agent.closed)
	}
}

func TestModelsReadsEffortsFromTheModelChoicesWithoutSwitching(t *testing.T) {
	// given
	// ... an agent whose model choices carry _meta.efforts, one of them empty
	agent := twoModels()
	agent.models = append(agent.models, fakeModel{id: "p/plain"})
	agent.metaEfforts = true

	// when
	// ... the launcher lists its models
	got, err := launcher(t, agent).Models("/work")

	// then
	// ... each model has the efforts it declared, the empty one has none, and no set_config_option was sent
	want := []core.Model{
		{ID: "p/fast", Name: "p/fast name", Description: "p/fast description", Efforts: []string{"low", "high"}},
		{ID: "p/deep", Name: "p/deep name", Description: "p/deep description", Efforts: []string{"low", "medium", "max"}},
		{ID: "p/plain", Name: "p/plain name", Description: "p/plain description"},
	}
	if err != nil || !reflect.DeepEqual(got, want) || len(agent.sets) != 0 {
		t.Fatalf("got %#v err %v sets %v", got, err, agent.sets)
	}
}

func TestModelsListsAModelTheAgentRefusesWithoutEfforts(t *testing.T) {
	// given
	// ... an agent that fails to switch to its second model, as when a provider's key command fails
	agent := twoModels()
	agent.setErr = map[string]error{"p/deep": errors.New("key command failed")}

	// when
	// ... the launcher lists its models
	got, err := launcher(t, agent).Models("/work")

	// then
	// ... both are listed, the second without efforts
	want := []core.Model{
		{ID: "p/fast", Name: "p/fast name", Description: "p/fast description", Efforts: []string{"low", "high"}},
		{ID: "p/deep", Name: "p/deep name", Description: "p/deep description"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v err %v", got, err)
	}
}

func TestModelsIsEmptyForAnAgentWithoutConfigOptions(t *testing.T) {
	// given
	// ... an agent that exposes no config options
	agent := &fakeAgent{}

	// when
	// ... the launcher lists its models
	got, err := launcher(t, agent).Models("/work")

	// then
	// ... the list is empty and there is no error
	if err != nil || len(got) != 0 {
		t.Fatalf("got %#v err %v", got, err)
	}
}

func TestStartReportsTheModelTheAgentRuns(t *testing.T) {
	// given
	// ... an agent on its first model and a session with no model of its own
	agent := twoModels()

	// when
	// ... the session starts
	_, out := start(t, agent, session())
	got := next(t, out)

	// then
	// ... Initialized names that model and nothing was set
	if want := (core.Initialized{Version: "9.9", CLISessionID: "acp-1", Model: "p/fast"}); got != want || len(agent.sets) != 0 {
		t.Fatalf("got %#v sets %v", got, agent.sets)
	}
}

func TestStartAppliesTheSessionsModelThenEffortBeforeTheFirstPrompt(t *testing.T) {
	// given
	// ... a session that recorded model p/deep and effort max, on an agent that starts on p/fast
	agent := twoModels()
	agent.onPrompt = replies(agent, "ok")
	s := session()
	s.Model, s.Effort = "p/deep", "max"

	// when
	// ... it starts and is sent a message
	ag, out := start(t, agent, s)
	got := next(t, out)
	ag.SendUser("hi", "u1")
	next(t, out)

	// then
	// ... the model was set before the effort (max only exists on p/deep), both before the prompt, and Initialized names p/deep
	want := []acpsdk.SetSessionConfigOptionValueId{set("m", "p/deep"), set("t", "max")}
	if wantEv := (core.Initialized{Version: "9.9", CLISessionID: "acp-1", Model: "p/deep"}); got != wantEv ||
		!reflect.DeepEqual(agent.sets, want) {
		t.Fatalf("got %#v sets %v", got, agent.sets)
	}
}

func TestStartFindsOptionsByIDWhenTheAgentNamesNoCategories(t *testing.T) {
	// given
	// ... an agent whose options carry the ids model and thought_level and no categories
	agent := twoModels()
	agent.byID = true
	s := session()
	s.Model = "p/deep"

	// when
	// ... a session recording p/deep starts
	_, out := start(t, agent, s)
	next(t, out)

	// then
	// ... the model option was set by its id
	if want := []acpsdk.SetSessionConfigOptionValueId{set("model", "p/deep")}; !reflect.DeepEqual(agent.sets, want) {
		t.Fatalf("sets %v", agent.sets)
	}
}

func TestStartContinuesWithANoticeWhenTheModelCannotBeSet(t *testing.T) {
	// given
	// ... an agent that refuses p/deep, and a session recording p/deep with effort high
	agent := twoModels()
	agent.setErr = map[string]error{"p/deep": errors.New("key command failed")}
	s := session()
	s.Model, s.Effort = "p/deep", "high"

	// when
	// ... it starts
	_, out := start(t, agent, s)
	got := events(t, out, 2)

	// then
	// ... Initialized names the model the agent kept, an info entry gives the agent's error, and the effort was still applied
	want := []core.Event{core.Initialized{Version: "9.9", CLISessionID: "acp-1", Model: "p/fast"},
		core.UserMessage{Entries: []core.Entry{{Kind: "info", Text: `model "p/deep" was not applied: key command failed; the agent stays on its current model`}}}}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(agent.sets, []acpsdk.SetSessionConfigOptionValueId{set("m", "p/deep"), set("t", "high")}) {
		t.Fatalf("got %#v sets %v", got, agent.sets)
	}
}

func TestResumedSessionAppliesTheSessionsModel(t *testing.T) {
	// given
	// ... an agent that reloads sessions, and a stored session recording p/deep
	agent := twoModels()
	agent.canLoad = true
	s := session()
	s.CLISessionID, s.Model = "old", "p/deep"

	// when
	// ... it is started again
	_, out := start(t, agent, s)
	got := next(t, out)

	// then
	// ... the model was set on the loaded session, and Initialized names it
	if want := (core.Initialized{Version: "9.9", CLISessionID: "old", Model: "p/deep"}); got != want || len(agent.sets) != 1 {
		t.Fatalf("got %#v sets %v", got, agent.sets)
	}
}

func TestSetModelThenSetEffortSendConfigOptionsAndReplyOK(t *testing.T) {
	// given
	// ... a started agent on p/fast
	agent := twoModels()
	ag, out := started(t, agent)

	// when
	// ... the model is switched and then an effort only the new model has is set
	modelID := ag.SetModel("p/deep")
	modelReply := next(t, out)
	effortID := ag.SetEffort("max")
	effortReply := next(t, out)

	// then
	// ... both reach the agent as set_config_option and each Reply is a success; max would have been refused on p/fast
	want := []acpsdk.SetSessionConfigOptionValueId{set("m", "p/deep"), set("t", "max")}
	if modelReply != (core.Reply{RequestID: modelID}) || effortReply != (core.Reply{RequestID: effortID}) || !reflect.DeepEqual(agent.sets, want) {
		t.Fatalf("replies %#v %#v sets %v", modelReply, effortReply, agent.sets)
	}
}

func TestSetModelRepliesWithTheAgentsError(t *testing.T) {
	// given
	// ... a started agent that fails switching to p/deep
	agent := twoModels()
	agent.setErr = map[string]error{"p/deep": errors.New("key command failed")}
	ag, out := started(t, agent)

	// when
	// ... the model is switched
	id := ag.SetModel("p/deep")
	got := next(t, out)

	// then
	// ... the Reply carries the agent's error
	if want := (core.Reply{RequestID: id, Err: "key command failed"}); got != want {
		t.Fatalf("got %#v", got)
	}
}
