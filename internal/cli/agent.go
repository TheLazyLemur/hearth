package cli

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"hearth/internal/core"
	"hearth/pkg/random"
)

// mcpServerName is the in-process MCP server the TUI hosts over the control
// protocol, like the desktop's "ccd_session" (spawn_task / dismiss_task).
const mcpServerName = "hearth"

var (
	_ core.Launcher = Launcher{}
	_ core.Agent    = (*process)(nil)
)

// Launcher starts `claude` CLI processes speaking stream-json, the same
// transport @anthropic-ai/claude-agent-sdk uses.
type Launcher struct{ Binary string }

// process is one CLI process. It translates the wire protocol to core events
// and back; all session state lives in core.App.
type process struct {
	mu    sync.Mutex
	w     io.WriteCloser
	cmd   *exec.Cmd
	tools []map[string]any // what the hearth MCP server lists
}

func cliArgs(s *core.Session) []string {
	args := []string{
		"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--permission-prompt-tool=stdio", "--include-partial-messages", "--replay-user-messages",
		"--setting-sources", "user,project,local",
		"--permission-mode", s.PermissionMode,
	}
	var allowed []string
	for _, t := range core.Tools {
		allowed = append(allowed, "mcp__"+mcpServerName+"__"+t["name"].(string))
	}
	// The built-in Agent tool is off: subagents are hearth sessions started with start_agent.
	args = append(args, "--allowedTools", strings.Join(allowed, ","), "--disallowedTools", "Agent")
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

func (l Launcher) Start(s *core.Session, tools []map[string]any, out chan<- core.AgentEvent) (core.Agent, error) {
	cmd := exec.Command(l.Binary, cliArgs(s)...)
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
	p := &process{w: stdin, cmd: cmd, tools: tools}
	p.request(map[string]any{"subtype": "initialize", "sdkMcpServers": []string{mcpServerName}})
	go func() {
		r := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				var m map[string]any
				if json.Unmarshal(line, &m) == nil {
					if ev := p.decode(m); ev != nil {
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

// Models starts a throwaway CLI and reads the model list from its initialize reply; no session is created.
func (l Launcher) Models(cwd string) ([]core.Model, error) {
	cmd := exec.Command(l.Binary, "--output-format", "stream-json", "--verbose", "--input-format", "stream-json")
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
	p := &process{w: stdin}
	id := p.request(map[string]any{"subtype": "initialize"})
	r := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["type"] == "control_response" {
			resp, _ := m["response"].(map[string]any)
			if str(resp["request_id"]) == id {
				if resp["subtype"] == "error" {
					return nil, errors.New(str(resp["error"]))
				}
				inner, _ := resp["response"].(map[string]any)
				return modelsFrom(inner), nil
			}
		}
		if err != nil {
			return nil, fmt.Errorf("claude exited without listing models: %w", err)
		}
	}
}

func modelsFrom(resp map[string]any) []core.Model {
	arr, _ := resp["models"].([]any)
	var out []core.Model
	for _, a := range arr {
		m, _ := a.(map[string]any)
		var efforts []string
		levels, _ := m["supportedEffortLevels"].([]any)
		for _, e := range levels {
			efforts = append(efforts, str(e))
		}
		out = append(out, core.Model{ID: str(m["value"]), Name: str(m["displayName"]), Description: str(m["description"]), Efforts: efforts})
	}
	return out
}

// decode turns one stdout line into a core event, or nil when core needn't see it.
func (p *process) decode(m map[string]any) core.Event {
	if str(m["parent_tool_use_id"]) != "" {
		return nil // subagent traffic; the parent's Task tool result summarises it
	}
	switch m["type"] {
	case "system":
		switch m["subtype"] {
		case "init":
			return core.Initialized{Version: str(m["claude_code_version"]), Model: str(m["model"]), CLISessionID: str(m["session_id"])}
		case "compact_boundary":
			return core.Compacted{}
		}
	case "stream_event":
		ev, _ := m["event"].(map[string]any)
		d, _ := ev["delta"].(map[string]any)
		if ev["type"] == "content_block_delta" && d["type"] == "text_delta" {
			return core.TextDelta{Text: str(d["text"])}
		}
	case "assistant":
		return core.AssistantMessage{Entries: entriesFromMessage(m)}
	case "user":
		if m["isReplay"] == true {
			return core.Consumed{UUID: str(m["uuid"])}
		}
		return core.UserMessage{Entries: entriesFromMessage(m)}
	case "result":
		b, _ := json.Marshal(m["errors"])
		return core.TurnEnded{OK: m["subtype"] == "success", Duration: time.Duration(num(m["duration_ms"])) * time.Millisecond,
			Failure: fmt.Sprintf("%v %s", m["subtype"], b)}
	case "control_request":
		return p.control(m)
	case "control_cancel_request":
		return core.PermissionCancelled{RequestID: str(m["request_id"])}
	case "control_response":
		r, _ := m["response"].(map[string]any)
		resp, _ := r["response"].(map[string]any)
		files, _ := resp["filesChanged"].([]any)
		reply := core.Reply{RequestID: str(r["request_id"]), Cancelled: resp["cancelled"] == true, Title: str(resp["title"]),
			Rewind: core.RewindPreview{CanRewind: resp["canRewind"] == true, Error: str(resp["error"]), Files: len(files),
				Insertions: int(num(resp["insertions"])), Deletions: int(num(resp["deletions"]))}}
		if r["subtype"] == "error" {
			reply.Err = cmp.Or(str(r["error"]), "request failed")
		}
		return reply
	}
	return nil
}

func (p *process) control(m map[string]any) core.Event {
	req, _ := m["request"].(map[string]any)
	id := str(m["request_id"])
	switch req["subtype"] {
	case "can_use_tool":
		in, _ := req["input"].(map[string]any)
		sug, _ := req["permission_suggestions"].([]any)
		return core.PermissionRequested{Perm: &core.Perm{RequestID: id, Tool: str(req["tool_name"]), Input: in, Suggestions: sug}}
	case "mcp_message":
		msg, _ := req["message"].(map[string]any)
		return p.mcp(id, msg)
	}
	p.respondErr(id, fmt.Sprintf("hearth does not support %v", req["subtype"]))
	return nil
}

// mcp answers JSON-RPC for the in-process hearth MCP server. Tool calls go
// to core as events whose Reply sends the result.
func (p *process) mcp(reqID string, m map[string]any) core.Event {
	reply := func(resp map[string]any) { p.respond(reqID, map[string]any{"mcp_response": resp}) }
	id, hasID := m["id"]
	if !hasID {
		reply(map[string]any{"jsonrpc": "2.0", "result": map[string]any{}, "id": 0}) // notification, as the SDK does
		return nil
	}
	params, _ := m["params"].(map[string]any)
	ok := func(result any) { reply(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
	text := func(t string, isErr bool) {
		ok(map[string]any{"content": []any{map[string]any{"type": "text", "text": t}}, "isError": isErr})
	}
	switch m["method"] {
	case "initialize":
		ok(map[string]any{"protocolVersion": params["protocolVersion"], "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": mcpServerName, "version": "0.1.0"}})
		return nil
	case "tools/list":
		tools := make([]map[string]any, len(p.tools))
		for i, t := range p.tools {
			// Claude Code's tool search would otherwise defer them, hiding them until the model searches.
			tools[i] = maps.Clone(t)
			tools[i]["_meta"] = map[string]any{"anthropic/alwaysLoad": true}
		}
		ok(map[string]any{"tools": tools})
		return nil
	case "tools/call":
		args, _ := params["arguments"].(map[string]any)
		return core.CallTool(str(params["name"]), args, text)
	}
	reply(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "method not found"}})
	return nil
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

func (p *process) request(req map[string]any) string {
	id := random.Hex(8)
	// A failed write means the CLI died; its Exited event reports that to the session.
	_ = p.send(map[string]any{"type": "control_request", "request_id": id, "request": req})
	return id
}

func (p *process) respond(id string, resp any) error {
	return p.send(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": id, "response": resp}})
}

func (p *process) respondErr(id, msg string) error {
	return p.send(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "error", "request_id": id, "error": msg}})
}

// SendUser may be called mid-turn: the CLI queues the message and injects it at the next
// tool boundary, echoing it back (with this uuid) once consumed.
func (p *process) SendUser(text, uuid string) error {
	return p.send(map[string]any{"type": "user", "uuid": uuid, "session_id": "", "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": text}})
}

func (p *process) Interrupt() string { return p.request(map[string]any{"subtype": "interrupt"}) }

func (p *process) CancelQueued(uuid string) string {
	return p.request(map[string]any{"subtype": "cancel_async_message", "message_uuid": uuid})
}

func (p *process) SetPermissionMode(mode string) string {
	return p.request(map[string]any{"subtype": "set_permission_mode", "mode": mode})
}

func (p *process) SetModel(model string) string {
	return p.request(map[string]any{"subtype": "set_model", "model": model})
}

func (p *process) SetEffort(effort string) string {
	return p.request(map[string]any{"subtype": "apply_flag_settings", "settings": map[string]any{"effortLevel": effort}})
}

func (p *process) Rename(title string) string {
	return p.request(map[string]any{"subtype": "rename_session", "title": title, "source": "host"})
}

func (p *process) GenerateTitle(description string) string {
	return p.request(map[string]any{"subtype": "generate_session_title", "description": description, "persist": true})
}

func (p *process) Rewind(userMessageID string, dryRun bool) string {
	req := map[string]any{"subtype": "rewind_files", "user_message_id": userMessageID}
	if dryRun {
		req["dry_run"] = true
	}
	return p.request(req)
}

func (p *process) Allow(requestID string, input map[string]any, permissions []any) error {
	resp := map[string]any{"behavior": "allow", "updatedInput": input}
	if len(permissions) > 0 {
		resp["updatedPermissions"] = permissions
	}
	return p.respond(requestID, resp)
}

func (p *process) Deny(requestID, message string) error {
	return p.respond(requestID, map[string]any{"behavior": "deny", "message": message})
}

// Close ends stdin so the CLI exits cleanly; it is killed if it lingers.
func (p *process) Close() {
	p.w.Close()
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	proc := p.cmd.Process
	time.AfterFunc(3*time.Second, func() { proc.Kill() })
}

func num(v any) float64 { f, _ := v.(float64); return f }

func str(v any) string { s, _ := v.(string); return s }
