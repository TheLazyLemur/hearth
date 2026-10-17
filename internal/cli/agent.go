package cli

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"claude-code-tui/internal/core"
	"claude-code-tui/pkg/random"
)

// mcpServerName is the in-process MCP server the TUI hosts over the control
// protocol, like the desktop's "ccd_session" (spawn_task / dismiss_task).
const mcpServerName = "tui_session"

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
	mu  sync.Mutex
	w   io.WriteCloser
	cmd *exec.Cmd
}

func cliArgs(s *core.Session) []string {
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

func (l Launcher) Start(s *core.Session, out chan<- core.AgentEvent) (core.Agent, error) {
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
	p := &process{w: stdin, cmd: cmd}
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
	p.respondErr(id, fmt.Sprintf("claude-tui does not support %v", req["subtype"]))
	return nil
}

var mcpTools = []map[string]any{
	{"name": "spawn_task", "description": "Suggest an out-of-scope follow-up (dead code, stale docs, missing tests, a confirmed TODO, a security issue noticed in passing) as a separate background task. The user sees a chip and can start it as its own session in a fresh worktree. Your turn continues. The prompt must stand alone: include file paths and context. Returns a task_id for dismiss_task.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"title", "prompt", "tldr"}, "properties": map[string]any{
			"title":  map[string]any{"type": "string", "description": "Under 60 chars, imperative, e.g. \"Fix stale README badge\"."},
			"prompt": map[string]any{"type": "string", "description": "Self-contained first message for the spawned session."},
			"tldr":   map[string]any{"type": "string", "description": "One or two plain sentences: why now, and what the session will do."},
			"cwd":    map[string]any{"type": "string", "description": "Optional absolute path to a different project root."},
		}}},
	{"name": "dismiss_task", "description": "Withdraw a task chip created with spawn_task that is now stale or superseded.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"task_id"}, "properties": map[string]any{
			"task_id": map[string]any{"type": "string"},
			"reason":  map[string]any{"type": "string"},
		}}},
}

// mcp answers JSON-RPC for the in-process tui_session MCP server. Tool calls go
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
		ok(map[string]any{"tools": mcpTools})
		return nil
	case "tools/call":
		args, _ := params["arguments"].(map[string]any)
		switch params["name"] {
		case "spawn_task":
			return core.TaskSuggested{Chip: core.Chip{Title: str(args["title"]), Prompt: str(args["prompt"]), Tldr: str(args["tldr"]), Cwd: str(args["cwd"])}, Reply: text}
		case "dismiss_task":
			return core.TaskDismissed{TaskID: str(args["task_id"]), Reply: text}
		}
		text(fmt.Sprintf("unknown tool %v", params["name"]), true)
		return nil
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
