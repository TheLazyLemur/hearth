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
// So far it runs one text turn at a time; every other Agent request replies with an error.
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

// Transcripts has no history to read until the agent can reload a session.
type Transcripts struct{}

func (Transcripts) Load(cwd, cliSessionID string) ([]core.Entry, error) {
	return nil, errors.New("this agent's session history is not restored yet")
}

// process is one ACP agent. It translates ACP to core events and back.
type process struct {
	sid   string // the hearth session
	out   chan<- core.AgentEvent
	conn  *acpsdk.ClientSideConnection
	tp    transport
	ready chan struct{} // closed once initialize and session/new are over, whether they worked or not

	acpSID acpsdk.SessionId // the agent's session; set before ready closes, empty if the handshake failed

	mu       sync.Mutex
	running  bool            // a turn's prompt is in flight, or about to be sent
	held     []heldMessage   // sent mid-turn, to go out as prompts when the turn ends, oldest first
	text     strings.Builder // the agent's text so far this message
	startErr error           // why the handshake failed
}

type heldMessage struct{ text, uuid string }

// Start spawns the agent; initialize and session/new run in the background and end in Initialized.
// tools are not served yet.
func (l Launcher) Start(s *core.Session, tools []map[string]any, out chan<- core.AgentEvent) (core.Agent, error) {
	if !filepath.IsAbs(s.Cwd) {
		return nil, fmt.Errorf("session folder %q is not absolute; ACP needs an absolute cwd", s.Cwd)
	}
	tp, err := l.open(s.Cwd)
	if err != nil {
		return nil, err
	}
	p := &process{sid: s.SessionID, out: out, tp: tp, ready: make(chan struct{})}
	p.conn = acpsdk.NewClientSideConnection(client{p}, tp.in, tp.out)
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

func (p *process) handshake(s *core.Session) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	init, err := p.conn.Initialize(ctx, acpsdk.InitializeRequest{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		ClientInfo:      &acpsdk.Implementation{Name: "hearth"},
	})
	if err != nil {
		return fmt.Errorf("initialize: %s", failureText(err))
	}
	if init.ProtocolVersion != acpsdk.ProtocolVersionNumber {
		return fmt.Errorf("initialize: agent speaks ACP version %d, hearth speaks %d", init.ProtocolVersion, acpsdk.ProtocolVersionNumber)
	}
	// No _meta.systemPrompt: hearth has no system-prompt text to append yet.
	// mcpServers is empty until hearth's tools are served over ACP.
	sess, err := p.conn.NewSession(ctx, acpsdk.NewSessionRequest{Cwd: s.Cwd, McpServers: []acpsdk.McpServer{}})
	if err != nil {
		return fmt.Errorf("session/new: %s", failureText(err))
	}
	if sess.SessionId == "" {
		return errors.New("session/new: the agent returned no sessionId")
	}
	p.acpSID = sess.SessionId
	ev := core.Initialized{CLISessionID: string(sess.SessionId)}
	if init.AgentInfo != nil {
		ev.Version = init.AgentInfo.Version
	}
	p.emit(ev)
	if s.CLISessionID != "" {
		// shortcut: no session/load yet, so a relaunch is a new conversation; drop once the agent advertises loadSession.
		p.emit(core.UserMessage{Entries: []core.Entry{{Kind: "info", Text: "history was not restored: this agent can't reload a session yet, so this is a fresh conversation"}}})
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
func (p *process) SetPermissionMode(string) string {
	return p.unsupported("permission modes")
}
func (p *process) SetModel(string) string  { return p.unsupported("switching models") }
func (p *process) SetEffort(string) string { return p.unsupported("setting an effort") }
func (p *process) Rename(string) string    { return p.unsupported("renaming a session") }
func (p *process) GenerateTitle(string) string {
	return p.unsupported("generating a title")
}
func (p *process) Rewind(string, bool) string { return p.unsupported("file rewind") }

// Allow and Deny answer permission requests, which hearth does not raise yet.
func (p *process) Allow(string, map[string]any, []any) error {
	return errors.New("this agent does not support permission requests yet")
}

func (p *process) Deny(string, string) error {
	return errors.New("this agent does not support permission requests yet")
}

// Close ends stdin so the agent exits cleanly; it is killed if it lingers.
func (p *process) Close() {
	p.tp.in.Close()
	time.AfterFunc(3*time.Second, p.tp.kill)
}

// client is hearth as the agent sees it: it offers no files or terminals, and
// answers nothing it hasn't built yet.
type client struct{ p *process }

var _ acpsdk.Client = client{}

func (c client) SessionUpdate(_ context.Context, n acpsdk.SessionNotification) error {
	if m := n.Update.AgentMessageChunk; m != nil && m.Content.Text != nil {
		c.p.mu.Lock()
		c.p.text.WriteString(m.Content.Text.Text)
		c.p.mu.Unlock()
		c.p.emit(core.TextDelta{Text: m.Content.Text.Text})
	}
	return nil
}

func (client) RequestPermission(context.Context, acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
	return acpsdk.RequestPermissionResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodSessionRequestPermission)
}

func (client) ReadTextFile(context.Context, acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
	return acpsdk.ReadTextFileResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodFsReadTextFile)
}

func (client) WriteTextFile(context.Context, acpsdk.WriteTextFileRequest) (acpsdk.WriteTextFileResponse, error) {
	return acpsdk.WriteTextFileResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodFsWriteTextFile)
}

func (client) CreateTerminal(context.Context, acpsdk.CreateTerminalRequest) (acpsdk.CreateTerminalResponse, error) {
	return acpsdk.CreateTerminalResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalCreate)
}

func (client) KillTerminal(context.Context, acpsdk.KillTerminalRequest) (acpsdk.KillTerminalResponse, error) {
	return acpsdk.KillTerminalResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalKill)
}

func (client) TerminalOutput(context.Context, acpsdk.TerminalOutputRequest) (acpsdk.TerminalOutputResponse, error) {
	return acpsdk.TerminalOutputResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalOutput)
}

func (client) ReleaseTerminal(context.Context, acpsdk.ReleaseTerminalRequest) (acpsdk.ReleaseTerminalResponse, error) {
	return acpsdk.ReleaseTerminalResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalRelease)
}

func (client) WaitForTerminalExit(context.Context, acpsdk.WaitForTerminalExitRequest) (acpsdk.WaitForTerminalExitResponse, error) {
	return acpsdk.WaitForTerminalExitResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalWaitForExit)
}
