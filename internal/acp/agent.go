// Package acp runs any Agent Client Protocol agent as a hearth backend: hearth spawns the agent
// and is its client over stdio.
package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"hearth/internal/core"
	"hearth/pkg/random"
)

var (
	_ core.Launcher    = Launcher{}
	_ core.Transcripts = Transcripts{}
	_ core.Agent       = (*process)(nil)
)

// Launcher starts an ACP agent: Binary with Args, speaking ACP on stdio.
// Model and effort changes, renaming, titles, rewinding and cancelling one queued message reply with an error so far.
type Launcher struct {
	Binary string
	Args   []string
	spawn  func(cwd string) (transport, error) // tests swap in pipes for a process
}

// transport is a started agent: its stdio, how to wait for it to exit and how to stop it.
type transport struct {
	in   io.WriteCloser
	out  io.Reader
	wait func() error
	kill func()
}

func (l Launcher) open(cwd string) (transport, error) {
	if !filepath.IsAbs(cwd) {
		return transport{}, fmt.Errorf("session folder %q is not absolute; ACP needs an absolute cwd", cwd)
	}
	if l.spawn != nil {
		return l.spawn(cwd)
	}
	cmd := exec.Command(l.Binary, l.Args...)
	cmd.Dir = cwd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return transport{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return transport{}, err
	}
	if err := cmd.Start(); err != nil {
		return transport{}, err
	}
	return transport{in: stdin, out: stdout, wait: cmd.Wait, kill: func() { cmd.Process.Kill() }}, nil
}

// Transcripts reads a session's history by having a short-lived agent reload it.
type Transcripts struct{ Launcher Launcher }

// Load starts the agent, replays the session through session/load and stops the agent again.
// It fails for an agent that can't reload a session.
func (t Transcripts) Load(cwd, cliSessionID string) ([]core.Entry, error) {
	tp, err := t.Launcher.open(cwd)
	if err != nil {
		return nil, err
	}
	p := newProcess(tp)
	go func() { // reaps the process
		<-p.conn.Done()
		tp.wait()
	}()
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	init, err := p.initialize(ctx)
	if err != nil {
		return nil, err
	}
	if !init.AgentCapabilities.LoadSession {
		return nil, errors.New("this agent's session history is not restored yet")
	}
	return p.load(ctx, acpsdk.SessionId(cliSessionID), cwd, []acpsdk.McpServer{}) // it runs no turn, so it needs no tools
}

// replay is a session's conversation as session/load streams it back.
type replay struct{ entries []core.Entry }

// add appends a message chunk, joining it to the entry before when that is the same speaker's.
func (r *replay) add(u acpsdk.SessionUpdate) {
	var kind string
	var c acpsdk.ContentBlock
	switch {
	case u.UserMessageChunk != nil:
		kind, c = "user", u.UserMessageChunk.Content
	case u.AgentMessageChunk != nil:
		kind, c = "assistant", u.AgentMessageChunk.Content
	case u.ToolCall != nil:
		r.entries = append(r.entries, toolEntry(u.ToolCall))
		return
	case u.ToolCallUpdate != nil:
		if e, ok := resultEntry(u.ToolCallUpdate); ok {
			r.entries = append(r.entries, e)
		}
		return
	}
	if c.Text == nil {
		return
	}
	if n := len(r.entries); n > 0 && r.entries[n-1].Kind == kind {
		r.entries[n-1].Text += c.Text.Text
		return
	}
	r.entries = append(r.entries, core.Entry{Kind: kind, Text: c.Text.Text})
}

// process is one ACP agent. It translates ACP to core events and back.
type process struct {
	sid   string // the hearth session
	out   chan<- core.AgentEvent
	conn  conn
	tp    transport
	ready chan struct{} // closed once initialize and session/new are over, whether they worked or not

	acpSID acpsdk.SessionId // the agent's session; set before ready closes, empty if the handshake failed

	tools []map[string]any // what hearth's MCP server lists
	cwd   string           // the session's folder; tool calls outside it always ask

	mu       sync.Mutex
	mode     string                                      // permission mode, deciding which tool calls ask
	perms    map[string]*pendingPerm                     // permission requests awaiting the user, by hearth's request id
	mcpID    string                                      // the id of hearth's MCP server in this session; empty when the agent has no MCP-over-ACP
	mcpConns map[acpsdk.UnstableMcpConnectionId]struct{} // MCP connections the agent opened to hearth's server
	running  bool                                        // a turn's prompt is in flight, or about to be sent
	held     []heldMessage                               // sent mid-turn, to go out as prompts when the turn ends, oldest first
	text     strings.Builder                             // the agent's text so far this message
	startErr error                                       // why the handshake failed
	replay   *replay                                     // set while session/load streams the history back; those updates are not live
}

type heldMessage struct{ text, uuid string }

// newProcess is the process of agent tp, with its connection open.
func newProcess(tp transport) *process {
	p := &process{tp: tp, perms: map[string]*pendingPerm{}, mcpConns: map[acpsdk.UnstableMcpConnectionId]struct{}{}}
	p.conn = conn{acpsdk.NewConnection(client{p}.handle, tp.in, tp.out)}
	return p
}

// Start spawns the agent; initialize and session/new run in the background and end in Initialized.
// tools is what hearth's MCP server lists, offered when the agent speaks MCP-over-ACP.
func (l Launcher) Start(s *core.Session, tools []map[string]any, out chan<- core.AgentEvent) (core.Agent, error) {
	tp, err := l.open(s.Cwd)
	if err != nil {
		return nil, err
	}
	p := newProcess(tp)
	p.sid, p.out, p.ready, p.tools, p.cwd, p.mode = s.SessionID, out, make(chan struct{}), tools, s.Cwd, s.PermissionMode
	go func() {
		defer close(p.ready)
		if err := p.handshake(s); err != nil {
			p.mu.Lock()
			p.startErr = err
			p.mu.Unlock()
			tp.kill()
		}
	}()
	go func() {
		<-p.conn.Done()
		werr := tp.wait()
		if werr == nil {
			werr = io.EOF
		}
		p.mu.Lock()
		if p.startErr != nil {
			werr = p.startErr
		}
		p.mu.Unlock()
		p.emit(core.Exited{Err: werr})
	}()
	return p, nil
}

// Models lists none until the agent's model options are read (a later slice); hearth tolerates an empty list.
func (l Launcher) Models(cwd string) ([]core.Model, error) { return nil, nil }

func (p *process) emit(ev core.Event) { p.out <- core.AgentEvent{SessionID: p.sid, Event: ev} }

func (p *process) initialize(ctx context.Context) (acpsdk.InitializeResponse, error) {
	init, err := p.conn.Initialize(ctx, acpsdk.InitializeRequest{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		ClientInfo:      &acpsdk.Implementation{Name: "hearth"},
	})
	if err != nil {
		return init, fmt.Errorf("initialize: %s", failureText(err))
	}
	if init.ProtocolVersion != acpsdk.ProtocolVersionNumber {
		return init, fmt.Errorf("initialize: agent speaks ACP version %d, hearth speaks %d", init.ProtocolVersion, acpsdk.ProtocolVersionNumber)
	}
	return init, nil
}

// load reloads the agent's session id and returns the conversation it replayed, which session/load
// sends before it answers.
func (p *process) load(ctx context.Context, id acpsdk.SessionId, cwd string, servers []acpsdk.McpServer) ([]core.Entry, error) {
	r := &replay{}
	p.mu.Lock()
	p.replay = r
	p.mu.Unlock()
	_, err := p.conn.LoadSession(ctx, acpsdk.LoadSessionRequest{SessionId: id, Cwd: cwd, McpServers: servers})
	p.mu.Lock()
	p.replay = nil
	p.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("session/load: %s", failureText(err))
	}
	return r.entries, nil
}

func (p *process) handshake(s *core.Session) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	init, err := p.initialize(ctx)
	if err != nil {
		return err
	}
	initialized := func(id acpsdk.SessionId) {
		p.acpSID = id
		ev := core.Initialized{CLISessionID: string(id)}
		if init.AgentInfo != nil {
			ev.Version = init.AgentInfo.Version
		}
		p.emit(ev)
	}
	servers := p.mcpServers(init)
	var notice string // why a session the agent has seen before starts fresh
	if s.CLISessionID != "" {
		if !init.AgentCapabilities.LoadSession {
			notice = "history was not restored: this agent can't reload a session yet, so this is a fresh conversation"
		} else if _, err := p.load(ctx, acpsdk.SessionId(s.CLISessionID), s.Cwd, servers); err != nil {
			notice = "history was not restored: " + err.Error() + ", so this is a fresh conversation"
		} else {
			// The replay is dropped: the session's entries already hold it, or EnsureLoaded loads it.
			initialized(acpsdk.SessionId(s.CLISessionID))
			return nil
		}
	}
	// No _meta.systemPrompt: hearth has no system-prompt text to append yet.
	sess, err := p.conn.NewSession(ctx, acpsdk.NewSessionRequest{Cwd: s.Cwd, McpServers: servers})
	if err != nil {
		return fmt.Errorf("session/new: %s", failureText(err))
	}
	if sess.SessionId == "" {
		return errors.New("session/new: the agent returned no sessionId")
	}
	initialized(sess.SessionId)
	if notice != "" {
		p.emit(core.UserMessage{Entries: []core.Entry{{Kind: "info", Text: notice}}})
	}
	return nil
}

// failureText is the agent's own words for err when it sent some.
func failureText(err error) string {
	var re *acpsdk.RequestError
	if errors.As(err, &re) {
		if d, ok := re.Data.(map[string]any); ok {
			if s, ok := d["error"].(string); ok && s != "" {
				return s
			}
		}
	}
	return err.Error()
}

// SendUser sends a prompt at once when idle. Mid-turn there is no steering yet: the message is
// held and sent as the next prompt when the turn ends.
func (p *process) SendUser(text, uuid string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		p.held = append(p.held, heldMessage{text, uuid})
		return nil
	}
	p.running = true
	go p.turns(text, uuid)
	return nil
}

// turns runs the prompt, then each message held meanwhile, until none is left.
func (p *process) turns(text, uuid string) {
	<-p.ready
	for p.acpSID != "" && p.turn(text, uuid) {
		p.mu.Lock()
		if len(p.held) == 0 {
			p.running = false
			p.mu.Unlock()
			return
		}
		next := p.held[0]
		p.held = p.held[1:]
		p.mu.Unlock()
		text, uuid = next.text, next.uuid
	}
}

// turn sends one prompt and reports how it ended. It returns false when the agent is gone,
// which Exited reports instead.
func (p *process) turn(text, uuid string) bool {
	p.emit(core.Consumed{UUID: uuid})
	started := time.Now()
	resp, err := p.conn.Prompt(context.Background(), acpsdk.PromptRequest{SessionId: p.acpSID, Prompt: []acpsdk.ContentBlock{acpsdk.TextBlock(text)}})
	select {
	case <-p.conn.Done():
		if err != nil {
			return false
		}
	default:
	}
	p.cancelPerms() // the turn is over, so nobody is left to answer
	// Prompt returns after every update sent before its response was handled, so the reply is whole.
	p.mu.Lock()
	msg := p.text.String()
	p.text.Reset()
	p.mu.Unlock()
	if msg != "" {
		p.emit(core.AssistantMessage{Entries: []core.Entry{{Kind: "assistant", Text: msg}}})
	}
	d := time.Since(started)
	switch {
	case err != nil:
		p.emit(core.TurnEnded{Duration: d, Failure: failureText(err)})
	case resp.StopReason == acpsdk.StopReasonEndTurn:
		p.emit(core.TurnEnded{OK: true, Duration: d})
	case resp.StopReason == acpsdk.StopReasonCancelled:
		p.emit(core.UserMessage{Entries: []core.Entry{{Kind: "interrupt"}}})
		p.emit(core.TurnEnded{Duration: d, Failure: "cancelled"})
	default:
		p.emit(core.TurnEnded{Duration: d, Failure: "stopped: " + string(resp.StopReason)})
	}
	return true
}

// Interrupt sends session/cancel; the turn then ends with the agent's cancelled stop reason.
func (p *process) Interrupt() string {
	id := random.Hex(8)
	go func() {
		<-p.ready
		r := core.Reply{RequestID: id}
		if p.acpSID == "" {
			r.Err = "the agent did not start"
		} else if err := p.conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: p.acpSID}); err != nil {
			r.Err = failureText(err)
		}
		p.cancelPerms() // ACP: a cancelled session answers its pending permission requests as cancelled
		p.emit(r)
	}()
	return id
}

// reply answers a request the agent can't serve. It is sent from a goroutine because core,
// which called us, is the one reading out.
func (p *process) reply(r core.Reply) string {
	r.RequestID = random.Hex(8)
	go p.emit(r)
	return r.RequestID
}

func (p *process) unsupported(what string) string {
	return p.reply(core.Reply{Err: "this agent does not support " + what + " yet"})
}

func (p *process) CancelQueued(string) string { return p.unsupported("cancelling one queued message") }
func (p *process) SetPermissionMode(mode string) string {
	p.mu.Lock()
	p.mode = mode
	p.mu.Unlock()
	return p.reply(core.Reply{})
}
func (p *process) SetModel(string) string  { return p.unsupported("switching models") }
func (p *process) SetEffort(string) string { return p.unsupported("setting an effort") }
func (p *process) Rename(string) string    { return p.unsupported("renaming a session") }
func (p *process) GenerateTitle(string) string {
	return p.unsupported("generating a title")
}
func (p *process) Rewind(string, bool) string { return p.unsupported("file rewind") }

// Close ends stdin so the agent exits cleanly; it is killed if it lingers.
func (p *process) Close() {
	p.tp.in.Close()
	time.AfterFunc(3*time.Second, p.tp.kill)
}
