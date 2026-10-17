package pi

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"claude-code-tui/internal/core"
	"claude-code-tui/pkg/random"
)

var (
	_ core.Launcher = Launcher{}
	_ core.Agent    = (*process)(nil)
)

// Launcher starts pi in RPC mode, gating its tool calls by the session's permission mode.
// pi has no file checkpoints or title generation; those requests reply with an error.
type Launcher struct{ Binary string }

// process is one pi process. It translates the RPC protocol to core events and back.
type process struct {
	mu      sync.Mutex
	w       io.WriteCloser
	channel io.Writer // to host.mjs: tool results and permission answers
	mode    string    // permission mode, deciding which tool calls ask
	cwd     string    // the session's folder; tool calls outside it always ask
	cmd     *exec.Cmd
	out     chan<- core.AgentEvent
	sid     string
	stateID string            // the get_state request whose response is Initialized
	prompts map[string]string // prompt request id -> message uuid, until pi answers it
	queued  []string          // uuids of sent messages pi has not read yet, oldest first
	// reader goroutine only
	started time.Time
	failure string // errorMessage of the run's last assistant message
}

func piArgs(s *core.Session) []string {
	args := []string{"--mode", "rpc"}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	if s.Effort != "" {
		args = append(args, "--thinking", s.Effort)
	}
	if s.CLISessionID != "" {
		args = append(args, "--session", s.CLISessionID)
	}
	return args
}

// pkgDir finds the pi package the Binary command runs, for host.mjs to import.
func (l Launcher) pkgDir() (string, error) {
	bin, err := exec.LookPath(l.Binary)
	if err != nil {
		return "", err
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return "", err
	}
	for d := filepath.Dir(bin); d != filepath.Dir(d); d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "package.json")); err == nil {
			return d, nil
		}
	}
	return "", fmt.Errorf("no package.json above %s", bin)
}

// Start runs pi through host.mjs, so the agent also gets core.Tools; their calls come back on fd 3.
func (l Launcher) Start(s *core.Session, out chan<- core.AgentEvent) (core.Agent, error) {
	pkg, err := l.pkgDir()
	if err != nil {
		return nil, fmt.Errorf("finding the pi package: %w", err)
	}
	tools, err := json.Marshal(core.Tools)
	if err != nil {
		return nil, err
	}
	// close-on-exec, as Go's own files are, so later child processes don't inherit the pair;
	// ExtraFiles still passes theirs to pi. ForkLock keeps a concurrent fork out of the gap.
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, err
	}
	ours, theirs := os.NewFile(uintptr(fds[0]), "tools"), os.NewFile(uintptr(fds[1]), "tools-pi")
	defer theirs.Close()
	started := false
	defer func() {
		if !started {
			ours.Close()
		}
	}()
	cmd := exec.Command("node", append([]string{"--input-type=module", "-e", hostJS, "--"}, piArgs(s)...)...)
	cmd.Dir = s.Cwd
	cmd.Env = append(os.Environ(), "CLAUDE_TUI_PI_PKG="+pkg, "CLAUDE_TUI_TOOLS="+string(tools))
	cmd.ExtraFiles = []*os.File{theirs} // fd 3
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	started = true
	p := &process{w: stdin, channel: ours, mode: s.PermissionMode, cwd: s.Cwd, cmd: cmd, out: out, sid: s.SessionID, prompts: map[string]string{}}
	go func() {
		defer ours.Close()
		p.serve(ours, func(ev core.Event) { out <- core.AgentEvent{SessionID: s.SessionID, Event: ev} })
	}()
	p.stateID = p.request(map[string]any{"type": "get_state"})
	go func() {
		// split on LF only: JSON strings may hold U+2028, which pi does not escape
		r := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				var m map[string]any
				if json.Unmarshal(line, &m) == nil {
					for _, ev := range p.decode(m) {
						out <- core.AgentEvent{SessionID: s.SessionID, Event: ev}
					}
				}
			}
			if err != nil {
				break
			}
		}
		werr := cmd.Wait()
		if werr == nil {
			werr = io.EOF
		}
		out <- core.AgentEvent{SessionID: s.SessionID, Event: core.Exited{Err: werr}}
	}()
	return p, nil
}

// thinkingLevels are pi's levels; pi clamps one a model lacks to the nearest it has.
var thinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh"}

// Models asks a throwaway pi, which writes no session, for the models it has credentials for.
func (l Launcher) Models(cwd string) ([]core.Model, error) {
	cmd := exec.Command(l.Binary, "--mode", "rpc", "--no-session")
	cmd.Dir = cwd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer cmd.Wait()
	defer cmd.Process.Kill()
	defer time.AfterFunc(30*time.Second, func() { cmd.Process.Kill() }).Stop()
	if _, err := stdin.Write([]byte(`{"id":"models","type":"get_available_models"}` + "\n")); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["id"] == "models" {
			if m["success"] != true {
				return nil, errors.New(str(m["error"]))
			}
			data, _ := m["data"].(map[string]any)
			return modelsFrom(data), nil
		}
		if err != nil {
			return nil, fmt.Errorf("pi exited without listing models: %w", err)
		}
	}
}

func modelsFrom(data map[string]any) []core.Model {
	arr, _ := data["models"].([]any)
	var out []core.Model
	for _, a := range arr {
		m, _ := a.(map[string]any)
		model := core.Model{ID: str(m["provider"]) + "/" + str(m["id"]), Name: str(m["name"]), Description: str(m["provider"])}
		if m["reasoning"] == true {
			model.Efforts = thinkingLevels
		}
		out = append(out, model)
	}
	return out
}

// decode turns one stdout record into the core events it means, if any.
func (p *process) decode(m map[string]any) []core.Event {
	switch m["type"] {
	case "response":
		return p.response(m)
	case "message_update":
		ev, _ := m["assistantMessageEvent"].(map[string]any)
		if ev["type"] == "text_delta" {
			return []core.Event{core.TextDelta{Text: str(ev["delta"])}}
		}
	case "message_end":
		msg, _ := m["message"].(map[string]any)
		switch msg["role"] {
		case "user":
			if uuid := p.consume(""); uuid != "" {
				return []core.Event{core.Consumed{UUID: uuid}}
			}
			return []core.Event{core.UserMessage{Entries: entries(msg)}} // not ours, e.g. from an extension
		case "assistant":
			p.failure = str(msg["errorMessage"])
			return []core.Event{core.AssistantMessage{Entries: entries(msg)}}
		case "toolResult":
			return []core.Event{core.UserMessage{Entries: entries(msg)}}
		}
	case "agent_start":
		if p.started.IsZero() {
			p.started = time.Now()
		}
	case "agent_settled":
		var d time.Duration
		if !p.started.IsZero() {
			d = time.Since(p.started)
		}
		failure := p.failure
		p.started, p.failure = time.Time{}, ""
		if m["aborted"] == true {
			return []core.Event{core.UserMessage{Entries: []core.Entry{{Kind: "interrupt"}}}, core.TurnEnded{Duration: d}}
		}
		return []core.Event{core.TurnEnded{OK: failure == "", Duration: d, Failure: failure}}
	case "compaction_end":
		if m["result"] != nil {
			return []core.Event{core.Compacted{}}
		}
	}
	return nil
}

func (p *process) response(m map[string]any) []core.Event {
	id, ok := str(m["id"]), m["success"] == true
	switch {
	case id == p.stateID:
		data, _ := m["data"].(map[string]any)
		model, _ := data["model"].(map[string]any)
		return []core.Event{core.Initialized{Model: str(model["provider"]) + "/" + str(model["id"]), CLISessionID: str(data["sessionId"])}}
	case m["command"] == "prompt":
		p.mu.Lock()
		uuid := p.prompts[id]
		delete(p.prompts, id)
		p.mu.Unlock()
		data, _ := m["data"].(map[string]any)
		if ok && data["disposition"] != "handled" {
			return nil // pi reads it later; message_end consumes it
		}
		// no run follows a handled or rejected prompt, so end the turn here.
		// shortcut: this also ends a run already in progress; fine while extension commands are rare.
		p.consume(uuid)
		return []core.Event{core.Consumed{UUID: uuid}, core.TurnEnded{OK: ok, Failure: str(m["error"])}}
	}
	r := core.Reply{RequestID: id}
	if !ok {
		r.Err = str(m["error"])
	}
	return []core.Event{r}
}

// consume removes uuid, or the oldest queued uuid when empty, and returns it ("" when none is queued).
func (p *process) consume(uuid string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if uuid == "" && len(p.queued) > 0 {
		uuid = p.queued[0]
	}
	if i := slices.Index(p.queued, uuid); i >= 0 {
		p.queued = slices.Delete(p.queued, i, i+1)
		return uuid
	}
	return ""
}

func (p *process) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err = p.w.Write(append(b, '\n'))
	return err
}

func (p *process) request(cmd map[string]any) string {
	id := random.Hex(8)
	cmd["id"] = id
	// A failed write means pi died; its Exited event reports that to the session.
	_ = p.send(cmd)
	return id
}

// reply answers a request pi can't serve. It is sent from a goroutine because core,
// which called us, is the one reading out.
func (p *process) reply(r core.Reply) string {
	r.RequestID = random.Hex(8)
	go func() { p.out <- core.AgentEvent{SessionID: p.sid, Event: r} }()
	return r.RequestID
}

func (p *process) unsupported(what string) string {
	return p.reply(core.Reply{Err: "pi does not support " + what})
}

// SendUser steers a running turn: pi injects the message after the current tool calls.
func (p *process) SendUser(text, uuid string) error {
	id := random.Hex(8)
	p.mu.Lock()
	p.queued = append(p.queued, uuid)
	p.prompts[id] = uuid
	p.mu.Unlock()
	return p.send(map[string]any{"id": id, "type": "prompt", "message": text, "streamingBehavior": "steer"})
}

func (p *process) Interrupt() string { return p.request(map[string]any{"type": "abort"}) }

func (p *process) CancelQueued(string) string { return p.unsupported("cancelling one queued message") }

func (p *process) SetPermissionMode(mode string) string {
	p.mu.Lock()
	p.mode = mode
	p.mu.Unlock()
	return p.reply(core.Reply{})
}

// SetModel takes pi's provider/id form, e.g. ollama/deepseek-v4.1-flash:cloud.
func (p *process) SetModel(model string) string {
	provider, id, ok := strings.Cut(model, "/")
	if !ok {
		return p.reply(core.Reply{Err: "pi models are provider/id, e.g. ollama/deepseek-v4.1-flash:cloud"})
	}
	return p.request(map[string]any{"type": "set_model", "provider": provider, "modelId": id})
}

func (p *process) SetEffort(effort string) string {
	return p.request(map[string]any{"type": "set_thinking_level", "level": effort})
}

func (p *process) Rename(title string) string {
	return p.request(map[string]any{"type": "set_session_name", "name": title})
}

// GenerateTitle succeeds with no title, so the session keeps its first-line title.
func (p *process) GenerateTitle(string) string { return p.reply(core.Reply{}) }

func (p *process) Rewind(string, bool) string { return p.unsupported("file rewind") }

// Allow runs the tool call; "always" rules aren't offered, since pi raises no suggestions.
func (p *process) Allow(requestID string, input map[string]any, _ []any) error {
	p.answer(map[string]any{"id": requestID, "allow": true, "input": input})
	return nil
}

func (p *process) Deny(requestID, message string) error {
	p.answer(map[string]any{"id": requestID, "allow": false, "text": message})
	return nil
}

// Close ends stdin so pi exits cleanly; it is killed if it lingers.
func (p *process) Close() {
	p.w.Close()
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	proc := p.cmd.Process
	time.AfterFunc(3*time.Second, func() { proc.Kill() })
}

func str(v any) string { s, _ := v.(string); return s }
