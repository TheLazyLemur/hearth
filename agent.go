package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// mcpServerName is the in-process MCP server the TUI hosts over the control
// protocol, like the desktop's "ccd_session" (spawn_task / dismiss_task).
const mcpServerName = "tui_session"

// agentMsg is one stdout line from a CLI process, or its exit (Msg == nil).
type agentMsg struct {
	SessionID string
	Msg       map[string]any
	Exit      error
}

// Agent is one `claude` CLI process speaking stream-json, the same transport
// @anthropic-ai/claude-agent-sdk uses. It only does I/O; all state lives in App.
type Agent struct {
	mu  sync.Mutex
	w   io.WriteCloser
	cmd *exec.Cmd
}

func cliBinary() string {
	if b := os.Getenv("CLAUDE_TUI_CLI"); b != "" {
		return b
	}
	return "claude"
}

func cliArgs(s *Session) []string {
	args := []string{
		"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--permission-prompt-tool=stdio", "--include-partial-messages", "--replay-user-messages",
		"--setting-sources", "user,project,local",
		"--permission-mode", s.PermissionMode,
		"--allowedTools", "mcp__" + mcpServerName + "__spawn_task,mcp__" + mcpServerName + "__dismiss_task",
	}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	if s.Effort != "" {
		args = append(args, "--effort", s.Effort)
	}
	if s.CLISessionID != "" {
		args = append(args, "--resume", s.CLISessionID)
	}
	return args
}

func startAgent(s *Session, out chan<- agentMsg) (*Agent, error) {
	cmd := exec.Command(cliBinary(), cliArgs(s)...)
	cmd.Dir = s.Cwd
	cmd.Env = append(os.Environ(), "CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=true")
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
	a := &Agent{w: stdin, cmd: cmd}
	a.request(map[string]any{"subtype": "initialize", "sdkMcpServers": []string{mcpServerName}})
	go func() {
		r := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				var m map[string]any
				if json.Unmarshal(line, &m) == nil {
					out <- agentMsg{SessionID: s.SessionID, Msg: m}
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
		out <- agentMsg{SessionID: s.SessionID, Exit: werr}
	}()
	return a, nil
}

func (a *Agent) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err = a.w.Write(append(b, '\n'))
	return err
}

func (a *Agent) request(req map[string]any) string {
	id := randomHex(8)
	// A failed write means the CLI died; its exit message reports that to the session.
	_ = a.send(map[string]any{"type": "control_request", "request_id": id, "request": req})
	return id
}

func (a *Agent) respond(id string, resp any) error {
	return a.send(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": id, "response": resp}})
}

func (a *Agent) respondErr(id, msg string) error {
	return a.send(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "error", "request_id": id, "error": msg}})
}

// userMessage may be sent mid-turn: the CLI queues it and injects it at the next
// tool boundary, echoing it back (with this uuid) once consumed.
func (a *Agent) userMessage(text, uuid string) error {
	return a.send(map[string]any{"type": "user", "uuid": uuid, "session_id": "", "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": text}})
}

// close ends stdin so the CLI exits cleanly; it is killed if it lingers.
func (a *Agent) close() {
	a.w.Close()
	if a.cmd == nil || a.cmd.Process == nil {
		return
	}
	p := a.cmd.Process
	time.AfterFunc(3*time.Second, func() { p.Kill() })
}

func randomHex(n int) string {
	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
