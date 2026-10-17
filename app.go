package main

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"time"
)

// Perm is a pending can_use_tool request awaiting the user's answer.
type Perm struct {
	RequestID   string
	Tool        string
	Input       map[string]any
	Suggestions []any
	Answers     map[string]any // AskUserQuestion: question text -> answer
}

// Chip is a task suggestion the model raised with spawn_task.
type Chip struct{ TaskID, Title, Prompt, Tldr, Cwd string }

// Live is per-session runtime state; none of it is persisted.
type Live struct {
	agent        *Agent
	entries      []Entry
	loaded       bool
	running      bool
	interrupted  bool // the running turn was interrupted by the user
	errored      bool
	perms        []*Perm
	chips        []*Chip
	queued       []Entry // sent to the CLI, not yet consumed (echoed back)
	pending      map[string]func(map[string]any)
	wantWorktree bool
	started      time.Time // current turn, for the spinner
	verb         string    // spinner verb for the current turn
	version      string    // CLI version and model, from system/init
	model        string
}

type App struct {
	store    Store
	sessions []*Session
	live     map[string]*Live
	agentCh  chan agentMsg
	notice   string
	now      func() int64
	launched string // launch directory, default folder for new sessions
}

func newApp(store Store) (*App, error) {
	ss, err := store.Load()
	if err != nil {
		return nil, err
	}
	wd, _ := os.Getwd()
	return &App{store: store, sessions: ss, live: map[string]*Live{}, agentCh: make(chan agentMsg, 256),
		now: func() int64 { return time.Now().UnixMilli() }, launched: wd}, nil
}

func (a *App) L(id string) *Live {
	l := a.live[id]
	if l == nil {
		l = &Live{pending: map[string]func(map[string]any){}}
		a.live[id] = l
	}
	return l
}

func (a *App) find(id string) *Session {
	for _, s := range a.sessions {
		if s.SessionID == id {
			return s
		}
	}
	return nil
}

func (a *App) save(s *Session) {
	if err := a.store.Save(s); err != nil {
		a.notice = "save failed: " + err.Error()
	}
}

// visible lists sessions for the sidebar: starred first, then most recent.
func (a *App) visible(showArchived bool) []*Session {
	var out []*Session
	for _, s := range a.sessions {
		if s.IsArchived == showArchived {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsStarred != out[j].IsStarred {
			return out[i].IsStarred
		}
		return out[i].LastActivityAt > out[j].LastActivityAt
	})
	return out
}

func (a *App) status(s *Session) string {
	l := a.live[s.SessionID]
	switch {
	case l == nil:
		return "idle"
	case len(l.perms) > 0:
		return "waiting"
	case l.running:
		return "running"
	case l.errored:
		return "error"
	}
	return "idle"
}

func (a *App) anyRunning() bool {
	for _, l := range a.live {
		if l.running {
			return true
		}
	}
	return false
}

func newUUID() string {
	h := randomHex(32)
	return h[:8] + "-" + h[8:12] + "-4" + h[13:16] + "-" + h[16:20] + "-" + h[20:]
}

func (a *App) newSession(dir string, worktree bool) *Session {
	t := a.now()
	s := &Session{SessionID: "local_" + newUUID(), Cwd: dir, OriginCwd: dir, PermissionMode: "default",
		CreatedAt: t, LastActivityAt: t}
	a.L(s.SessionID).wantWorktree = worktree
	a.L(s.SessionID).loaded = true
	a.sessions = append([]*Session{s}, a.sessions...)
	a.save(s)
	return s
}

func (a *App) ensureLoaded(s *Session) {
	l := a.L(s.SessionID)
	if l.loaded || s.CLISessionID == "" {
		return
	}
	l.loaded = true
	es, err := loadTranscript(transcriptPath(s.Cwd, s.CLISessionID))
	if err != nil {
		l.entries = append(l.entries, Entry{Kind: "error", Text: "transcript unavailable: " + err.Error()})
		return
	}
	l.entries = es
}

func (a *App) ensureAgent(s *Session) (*Agent, error) {
	l := a.L(s.SessionID)
	if l.agent != nil {
		return l.agent, nil
	}
	ag, err := startAgent(s, a.agentCh)
	if err != nil {
		return nil, err
	}
	l.agent, l.errored = ag, false
	return ag, nil
}

func (a *App) send(s *Session, text string) error {
	l := a.L(s.SessionID)
	if l.wantWorktree {
		if err := createWorktree(s, text); err != nil {
			return err
		}
		l.wantWorktree = false
	}
	if s.Title == "" {
		s.Title, s.TitleSource = truncate(firstLines(text, 1), 50), "auto"
	}
	ag, err := a.ensureAgent(s)
	if err != nil {
		return err
	}
	id := newUUID()
	if err := ag.userMessage(text, id); err != nil {
		return err
	}
	l.queued = append(l.queued, Entry{Kind: "user", Text: text, UUID: id})
	startTurn(l)
	s.LastActivityAt = a.now()
	a.save(s)
	return nil
}

// call sends a control request and runs cb with the CLI's response.
func (a *App) call(s *Session, req map[string]any, cb func(map[string]any)) error {
	ag, err := a.ensureAgent(s)
	if err != nil {
		return err
	}
	a.L(s.SessionID).pending[ag.request(req)] = cb
	return nil
}

// interrupt stops the current turn; queued messages stay queued and run next.
func (a *App) interrupt(s *Session) {
	if l := a.live[s.SessionID]; l != nil && l.agent != nil && l.running {
		l.interrupted = true
		l.agent.request(map[string]any{"subtype": "interrupt"})
	}
}

// interruptAndSend stops the current turn and sends text as the next one.
func (a *App) interruptAndSend(s *Session, text string) error {
	a.interrupt(s)
	return a.send(s, text)
}

// unqueue pulls the newest still-queued message back; done gets its text once the CLI confirms.
func (a *App) unqueue(s *Session, done func(string)) error {
	l := a.L(s.SessionID)
	if len(l.queued) == 0 || l.agent == nil {
		return fmt.Errorf("nothing queued")
	}
	e := l.queued[len(l.queued)-1]
	return a.call(s, map[string]any{"subtype": "cancel_async_message", "message_uuid": e.UUID}, func(r map[string]any) {
		if r["cancelled"] != true {
			a.notice = "too late: Claude already read that message"
			return
		}
		for i, q := range l.queued {
			if q.UUID == e.UUID {
				l.queued = append(l.queued[:i], l.queued[i+1:]...)
				break
			}
		}
		done(e.Text)
	})
}

// liveRequest pushes a setting to a running CLI; the saved value applies at the next spawn regardless.
func (a *App) liveRequest(s *Session, req map[string]any) {
	if l := a.live[s.SessionID]; l != nil && l.agent != nil {
		l.pending[l.agent.request(req)] = func(map[string]any) {}
	}
	s.LastActivityAt = a.now()
	a.save(s)
}

func (a *App) setMode(s *Session, mode string) {
	s.PermissionMode = mode
	a.liveRequest(s, map[string]any{"subtype": "set_permission_mode", "mode": mode})
}

func (a *App) setModel(s *Session, model string) {
	s.Model = model
	a.liveRequest(s, map[string]any{"subtype": "set_model", "model": model})
}

func (a *App) setEffort(s *Session, effort string) {
	s.Effort = effort
	if effort != "" {
		a.liveRequest(s, map[string]any{"subtype": "apply_flag_settings", "settings": map[string]any{"effortLevel": effort}})
	} else {
		a.save(s)
	}
}

func (a *App) rename(s *Session, title string) {
	s.Title, s.TitleSource = title, "user"
	if l := a.live[s.SessionID]; l != nil && l.agent != nil {
		l.pending[l.agent.request(map[string]any{"subtype": "rename_session", "title": title, "source": "host"})] = func(map[string]any) {}
	}
	a.save(s)
}

// answerPerm resolves the oldest pending permission request for s.
func (a *App) answerPerm(s *Session, allow, always bool) error {
	l := a.L(s.SessionID)
	if len(l.perms) == 0 || l.agent == nil {
		return fmt.Errorf("no pending permission request")
	}
	p := l.perms[0]
	l.perms = l.perms[1:]
	if !allow {
		return l.agent.respond(p.RequestID, map[string]any{"behavior": "deny", "message": "The user denied this tool use."})
	}
	input := p.Input
	if p.Answers != nil {
		input = map[string]any{}
		for k, v := range p.Input {
			input[k] = v
		}
		input["answers"] = p.Answers
	}
	resp := map[string]any{"behavior": "allow", "updatedInput": input}
	if always && len(p.Suggestions) > 0 {
		resp["updatedPermissions"] = p.Suggestions
	}
	return l.agent.respond(p.RequestID, resp)
}

func (a *App) deleteSession(s *Session) error {
	l := a.L(s.SessionID)
	if l.agent != nil {
		l.agent.close()
		l.agent = nil
	}
	if s.WorktreePath != "" {
		if err := removeWorktree(s); err != nil {
			return fmt.Errorf("kept session, worktree not removed: %w", err)
		}
	}
	if err := a.store.Delete(s.SessionID); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(a.live, s.SessionID)
	for i, x := range a.sessions {
		if x == s {
			a.sessions = append(a.sessions[:i], a.sessions[i+1:]...)
			break
		}
	}
	return nil
}

// acceptChip starts the suggested task as its own session in a fresh worktree, like the desktop.
func (a *App) acceptChip(parent *Session, i int) (*Session, error) {
	l := a.L(parent.SessionID)
	if i < 0 || i >= len(l.chips) {
		return nil, fmt.Errorf("no task %d", i+1)
	}
	c := l.chips[i]
	dir := c.Cwd
	if dir == "" {
		dir = parent.OriginCwd
	}
	_, gitErr := git(dir, "rev-parse", "--show-toplevel")
	s := a.newSession(dir, gitErr == nil)
	s.Title, s.TitleSource = c.Title, "task"
	s.Model, s.Effort, s.PermissionMode = parent.Model, parent.Effort, parent.PermissionMode
	s.SpawnedFrom = &SpawnedFrom{SessionID: parent.SessionID, TaskID: c.TaskID, Title: c.Title}
	l.chips = append(l.chips[:i], l.chips[i+1:]...)
	return s, a.send(s, c.Prompt)
}

func (a *App) dismissChip(s *Session, i int) {
	l := a.L(s.SessionID)
	if i >= 0 && i < len(l.chips) {
		l.chips = append(l.chips[:i], l.chips[i+1:]...)
	}
}

func (a *App) closeAll() {
	for _, l := range a.live {
		if l.agent != nil {
			l.agent.close()
		}
	}
}

// handle applies one CLI message to app state. Runs on the UI goroutine only.
func (a *App) handle(m agentMsg) {
	s := a.find(m.SessionID)
	if s == nil {
		return
	}
	l := a.L(s.SessionID)
	if m.Msg == nil {
		l.agent = nil
		l.perms = nil
		for _, q := range l.queued {
			l.entries = append(l.entries, Entry{Kind: "error", Text: "not delivered (Claude Code exited): " + q.Text})
		}
		l.queued = nil
		if l.running {
			l.running, l.errored = false, true
			l.entries = append(l.entries, Entry{Kind: "error", Text: fmt.Sprintf("Claude Code exited: %v", m.Exit)})
		}
		return
	}
	msg := m.Msg
	if p, _ := msg["parent_tool_use_id"].(string); p != "" {
		return // subagent traffic; the parent's Task tool result summarises it
	}
	switch msg["type"] {
	case "system":
		switch msg["subtype"] {
		case "init":
			l.version, l.model = str(msg["claude_code_version"]), str(msg["model"])
			if id := str(msg["session_id"]); id != "" && id != s.CLISessionID {
				s.CLISessionID = id
				a.save(s)
			}
		case "compact_boundary":
			l.entries = append(l.entries, Entry{Kind: "info", Text: "✻ Conversation compacted"})
		}
	case "stream_event":
		ev, _ := msg["event"].(map[string]any)
		d, _ := ev["delta"].(map[string]any)
		if ev["type"] == "content_block_delta" && d["type"] == "text_delta" {
			if n := len(l.entries); n > 0 && l.entries[n-1].streaming {
				l.entries[n-1].Text += str(d["text"])
			} else {
				l.entries = append(l.entries, Entry{Kind: "assistant", Text: str(d["text"]), streaming: true})
			}
		}
	case "assistant":
		for n := len(l.entries); n > 0 && l.entries[n-1].streaming; n-- {
			l.entries = l.entries[:n-1]
		}
		l.entries = append(l.entries, entriesFromMessage(msg)...)
	case "user":
		if msg["isReplay"] == true {
			a.deliver(l, str(msg["uuid"]))
			return
		}
		l.entries = append(l.entries, entriesFromMessage(msg)...)
	case "result":
		l.running = false
		s.CompletedTurns++
		s.LastActivityAt = a.now()
		interrupted := l.interrupted
		l.interrupted = false
		switch {
		case interrupted && msg["subtype"] != "success":
			// the CLI already emitted "[Request interrupted by user]", shown as an interrupt entry
		case msg["subtype"] == "success":
			l.entries = append(l.entries, Entry{Kind: "info", Text: fmt.Sprintf("✻ %s for %s · done %s",
				doneVerbs[rand.IntN(len(doneVerbs))], fmtDur(time.Duration(num(msg["duration_ms"]))*time.Millisecond), time.Now().Format("3:04 PM"))})
		default:
			b, _ := json.Marshal(msg["errors"])
			l.entries = append(l.entries, Entry{Kind: "error", Text: fmt.Sprintf("%v %s", msg["subtype"], b)})
		}
		if s.TitleSource == "auto" && s.CompletedTurns == 1 {
			a.requestTitle(s, l)
		}
		a.save(s)
	case "control_request":
		a.handleControl(s, l, msg)
	case "control_cancel_request":
		id := str(msg["request_id"])
		for i, p := range l.perms {
			if p.RequestID == id {
				l.perms = append(l.perms[:i], l.perms[i+1:]...)
				break
			}
		}
	case "control_response":
		r, _ := msg["response"].(map[string]any)
		id := str(r["request_id"])
		cb, ok := l.pending[id]
		if !ok {
			return // our own responses echoed back by --replay-user-messages
		}
		delete(l.pending, id)
		if r["subtype"] == "error" {
			a.notice = "Claude Code: " + str(r["error"])
			return
		}
		resp, _ := r["response"].(map[string]any)
		cb(resp)
	}
}

// startTurn marks a turn running; a message steered into a running turn keeps its spinner.
func startTurn(l *Live) {
	if !l.running {
		l.running, l.started, l.verb = true, time.Now(), spinnerVerbs[rand.IntN(len(spinnerVerbs))]
	}
}

// fmtDur matches the CLI's "9s" / "1m 5s" turn timings.
func fmtDur(d time.Duration) string {
	sec := int(d.Round(time.Second) / time.Second)
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%dm %ds", sec/60, sec%60)
}

// deliver moves a queued message into the transcript at the point the CLI consumed it.
func (a *App) deliver(l *Live, uuid string) {
	for i, q := range l.queued {
		if q.UUID == uuid {
			l.queued = append(l.queued[:i], l.queued[i+1:]...)
			l.entries = append(l.entries, q)
			startTurn(l)
			return
		}
	}
}

func (a *App) requestTitle(s *Session, l *Live) {
	var first string
	for _, e := range l.entries {
		if e.Kind == "user" {
			first = e.Text
			break
		}
	}
	if first == "" || l.agent == nil {
		return
	}
	l.pending[l.agent.request(map[string]any{"subtype": "generate_session_title", "description": first, "persist": true})] =
		func(r map[string]any) {
			if t := str(r["title"]); t != "" && s.TitleSource == "auto" {
				s.Title = t
				a.save(s)
			}
		}
}

func (a *App) handleControl(s *Session, l *Live, msg map[string]any) {
	req, _ := msg["request"].(map[string]any)
	id := str(msg["request_id"])
	if l.agent == nil {
		panic("control request from a session without an agent")
	}
	switch req["subtype"] {
	case "can_use_tool":
		in, _ := req["input"].(map[string]any)
		sug, _ := req["permission_suggestions"].([]any)
		p := &Perm{RequestID: id, Tool: str(req["tool_name"]), Input: in, Suggestions: sug}
		if p.Tool == "AskUserQuestion" {
			p.Answers = map[string]any{}
		}
		l.perms = append(l.perms, p)
	case "mcp_message":
		m, _ := req["message"].(map[string]any)
		l.agent.respond(id, map[string]any{"mcp_response": a.mcp(s, l, m)})
	default:
		l.agent.respondErr(id, fmt.Sprintf("claude-tui does not support %v", req["subtype"]))
	}
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

// mcp answers JSON-RPC for the in-process tui_session MCP server.
func (a *App) mcp(s *Session, l *Live, m map[string]any) map[string]any {
	id, hasID := m["id"]
	if !hasID {
		return map[string]any{"jsonrpc": "2.0", "result": map[string]any{}, "id": 0} // notification, as the SDK does
	}
	params, _ := m["params"].(map[string]any)
	ok := func(result any) map[string]any { return map[string]any{"jsonrpc": "2.0", "id": id, "result": result} }
	text := func(t string, isErr bool) map[string]any {
		return ok(map[string]any{"content": []any{map[string]any{"type": "text", "text": t}}, "isError": isErr})
	}
	switch m["method"] {
	case "initialize":
		return ok(map[string]any{"protocolVersion": params["protocolVersion"], "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": mcpServerName, "version": "0.1.0"}})
	case "tools/list":
		return ok(map[string]any{"tools": mcpTools})
	case "tools/call":
		args, _ := params["arguments"].(map[string]any)
		switch params["name"] {
		case "spawn_task":
			c := &Chip{TaskID: "task_" + randomHex(8), Title: str(args["title"]), Prompt: str(args["prompt"]), Tldr: str(args["tldr"]), Cwd: str(args["cwd"])}
			if c.Title == "" || c.Prompt == "" {
				return text("title and prompt are required", true)
			}
			l.chips = append(l.chips, c)
			return text(fmt.Sprintf("Suggested as %s. The user sees a chip and may start it; continue your turn.", c.TaskID), false)
		case "dismiss_task":
			for i, c := range l.chips {
				if c.TaskID == str(args["task_id"]) {
					l.chips = append(l.chips[:i], l.chips[i+1:]...)
					return text("Withdrawn.", false)
				}
			}
			return text("That task was already started or dismissed; do not retry.", false)
		}
		return text(fmt.Sprintf("unknown tool %v", params["name"]), true)
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "method not found"}}
}

func num(v any) float64 { f, _ := v.(float64); return f }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// rewindTargets lists user turns that have a CLI UUID, in order.
func rewindTargets(l *Live) []Entry {
	var out []Entry
	for _, e := range l.entries {
		if e.Kind == "user" && e.UUID != "" {
			out = append(out, e)
		}
	}
	return out
}

func shortPath(p string) string {
	if home, _ := os.UserHomeDir(); home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}
