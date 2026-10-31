package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"hearth/internal/core"
	"hearth/pkg/random"
)

// mcpServerName is hearth's MCP server in an agent's session; Claude's tools are mcp__hearth__<tool>.
const mcpServerName = "hearth"

// conn is the connection to the agent. It is an acpsdk.Connection rather than a ClientSideConnection
// because the SDK never dispatches an inbound mcp/message: client.handle takes every inbound call instead.
type conn struct{ *acpsdk.Connection }

func (c conn) Initialize(ctx context.Context, r acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	return acpsdk.SendRequest[acpsdk.InitializeResponse](c.Connection, ctx, acpsdk.AgentMethodInitialize, r)
}

func (c conn) NewSession(ctx context.Context, r acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	return acpsdk.SendRequest[acpsdk.NewSessionResponse](c.Connection, ctx, acpsdk.AgentMethodSessionNew, r)
}

func (c conn) LoadSession(ctx context.Context, r acpsdk.LoadSessionRequest) (acpsdk.LoadSessionResponse, error) {
	return acpsdk.SendRequest[acpsdk.LoadSessionResponse](c.Connection, ctx, acpsdk.AgentMethodSessionLoad, r)
}

func (c conn) SetConfigOption(ctx context.Context, r acpsdk.SetSessionConfigOptionRequest) (acpsdk.SetSessionConfigOptionResponse, error) {
	return acpsdk.SendRequest[acpsdk.SetSessionConfigOptionResponse](c.Connection, ctx, acpsdk.AgentMethodSessionSetConfigOption, r)
}

func (c conn) CloseSession(ctx context.Context, r acpsdk.CloseSessionRequest) (acpsdk.CloseSessionResponse, error) {
	return acpsdk.SendRequest[acpsdk.CloseSessionResponse](c.Connection, ctx, acpsdk.AgentMethodSessionClose, r)
}

func (c conn) Prompt(ctx context.Context, r acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	return acpsdk.SendRequest[acpsdk.PromptResponse](c.Connection, ctx, acpsdk.AgentMethodSessionPrompt, r)
}

// steerRequest is _session/steering, the extension claude-agent-acp and codex-acp share for a message into a running turn.
type steerRequest struct {
	SessionId acpsdk.SessionId      `json:"sessionId"`
	Prompt    []acpsdk.ContentBlock `json:"prompt"`
	Meta      map[string]any        `json:"_meta"`
}

type steerResponse struct {
	Outcome string `json:"outcome"` // injected, startedNewTurn, promptRequired or failed
}

func (c conn) Steer(ctx context.Context, r steerRequest) (steerResponse, error) {
	return acpsdk.SendRequest[steerResponse](c.Connection, ctx, "_session/steering", r)
}

func (c conn) Cancel(ctx context.Context, n acpsdk.CancelNotification) error {
	return c.SendNotification(ctx, acpsdk.AgentMethodSessionCancel, n)
}

// client is hearth as the agent sees it. It offers no files or terminals: those calls, and any other
// the agent makes that hearth hasn't built, get method-not-found.
type client struct{ p *process }

func (c client) handle(ctx context.Context, method string, params json.RawMessage) (any, *acpsdk.RequestError) {
	switch method {
	case acpsdk.ClientMethodSessionUpdate:
		return serve(params, func(n acpsdk.SessionNotification) (any, error) { return nil, c.p.sessionUpdate(n) })
	case acpsdk.ClientMethodSessionRequestPermission:
		return serve(params, func(r acpsdk.RequestPermissionRequest) (any, error) { return c.p.requestPermission(ctx, r) })
	case acpsdk.ClientMethodMcpConnect:
		return serve(params, func(r acpsdk.UnstableConnectMcpRequest) (any, error) { return c.p.mcpConnect(r) })
	case acpsdk.ClientMethodMcpDisconnect:
		return serve(params, func(r acpsdk.UnstableDisconnectMcpRequest) (any, error) { return c.p.mcpDisconnect(r) })
	case acpsdk.ClientMethodMcpMessage:
		return serve(params, func(r acpsdk.UnstableMessageMcpRequest) (any, error) { return c.p.mcpMessage(ctx, r) })
	}
	return nil, acpsdk.NewMethodNotFound(method)
}

// serve decodes an inbound call's params as P and runs f on them.
func serve[P any](raw json.RawMessage, f func(P) (any, error)) (any, *acpsdk.RequestError) {
	var params P
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, acpsdk.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	res, err := f(params)
	var re *acpsdk.RequestError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &re):
		return nil, re
	}
	return nil, acpsdk.NewInternalError(map[string]any{"error": err.Error()})
}

func (p *process) sessionUpdate(n acpsdk.SessionNotification) error {
	u := n.Update
	p.mu.Lock()
	r := p.replay
	p.mu.Unlock()
	if r != nil {
		r.add(u)
		return nil
	}
	switch {
	case u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil:
		t := u.AgentMessageChunk.Content.Text.Text
		p.mu.Lock()
		p.text.WriteString(t)
		p.mu.Unlock()
		p.emit(core.TextDelta{Text: t})
	case u.ToolCall != nil:
		// The text so far lands with the call, so the transcript keeps the order the agent worked in.
		p.mu.Lock()
		msg := p.text.String()
		p.text.Reset()
		p.mu.Unlock()
		var entries []core.Entry
		if msg != "" {
			entries = append(entries, core.Entry{Kind: "assistant", Text: msg})
		}
		p.emit(core.AssistantMessage{Entries: append(entries, toolEntry(u.ToolCall))})
	case u.ToolCallUpdate != nil:
		if e, ok := resultEntry(u.ToolCallUpdate); ok {
			p.emit(core.UserMessage{Entries: []core.Entry{e}})
		}
	}
	return nil
}

// toolName is what the agent calls a tool: _meta.toolName (grimalkin), _meta.claudeCode.toolName
// (claude-agent-acp), else the call's title.
func toolName(meta map[string]any, title string) string {
	if s, _ := meta["toolName"].(string); s != "" {
		return s
	}
	cc, _ := meta["claudeCode"].(map[string]any)
	if s, _ := cc["toolName"].(string); s != "" {
		return s
	}
	return title
}

// toolInput is a tool call's rawInput as the arguments map the rest of hearth shows. An input that
// isn't an object is kept whole under "input".
func toolInput(raw any) map[string]any {
	switch in := raw.(type) {
	case nil:
		return nil
	case map[string]any:
		return in
	}
	return map[string]any{"input": raw}
}

func toolEntry(c *acpsdk.SessionUpdateToolCall) core.Entry {
	name, in := toolName(c.Meta, c.Title), toolInput(c.RawInput)
	text := name + "()"
	if len(in) > 0 {
		text = name + "(" + core.SummariseInput(in) + ")"
	}
	return core.Entry{Kind: "tool", Text: text, ID: string(c.ToolCallId)}
}

// resultEntry is the result of a tool call that finished, as its update carries it; not every update does.
func resultEntry(u *acpsdk.SessionToolCallUpdate) (core.Entry, bool) {
	if u.Status == nil || (*u.Status != acpsdk.ToolCallStatusCompleted && *u.Status != acpsdk.ToolCallStatusFailed) {
		return core.Entry{}, false
	}
	var parts []string
	for _, c := range u.Content {
		if c.Content != nil && c.Content.Content.Text != nil {
			parts = append(parts, c.Content.Content.Text.Text)
		}
	}
	return core.Entry{Kind: "result", Text: core.FirstLines(strings.Join(parts, "\n"), 4), ID: string(u.ToolCallId), Err: *u.Status == acpsdk.ToolCallStatusFailed}, true
}

// pendingPerm is a permission request the user hasn't answered.
type pendingPerm struct {
	options []acpsdk.PermissionOption
	answer  chan acpsdk.RequestPermissionOutcome // holds the one outcome the agent gets
}

var (
	allowKinds  = []acpsdk.PermissionOptionKind{acpsdk.PermissionOptionKindAllowOnce, acpsdk.PermissionOptionKindAllowAlways}
	rejectKinds = []acpsdk.PermissionOptionKind{acpsdk.PermissionOptionKindRejectOnce, acpsdk.PermissionOptionKindRejectAlways}
)

// selectOption is the outcome choosing the first option of the first kind in kinds that has one.
func selectOption(options []acpsdk.PermissionOption, kinds []acpsdk.PermissionOptionKind) (acpsdk.RequestPermissionOutcome, bool) {
	for _, k := range kinds {
		for _, o := range options {
			if o.Kind == k {
				return acpsdk.NewRequestPermissionOutcomeSelected(o.OptionId), true
			}
		}
	}
	return acpsdk.NewRequestPermissionOutcomeCancelled(), false
}

// hearthTool reports whether name is one of hearth's own tools, bare or as Claude names it.
func hearthTool(name string) bool {
	name = strings.TrimPrefix(name, "mcp__"+mcpServerName+"__")
	return slices.ContainsFunc(core.Tools, func(t map[string]any) bool { return t["name"] == name })
}

// gate is the tool name and path core.Permit judges. Permit knows pi's tool names, so an ACP tool of kind
// read, search or edit counts as pi's read, grep or edit, but only when its input names the path it works on:
// a kind alone, which the agent picks, doesn't say where.
func gate(name string, kind *acpsdk.ToolKind, in map[string]any, locations []acpsdk.ToolCallLocation) (tool, path string) {
	path, _ = in["path"].(string)
	if path == "" {
		path, _ = in["file_path"].(string)
	}
	if path == "" && len(locations) > 0 {
		path = locations[0].Path
	}
	if kind != nil && path != "" {
		switch *kind {
		case acpsdk.ToolKindRead:
			return "read", path
		case acpsdk.ToolKindSearch:
			return "grep", path
		case acpsdk.ToolKindEdit:
			return "edit", path
		}
	}
	return name, path
}

// requestPermission answers session/request_permission: by the session's mode when that settles it,
// otherwise by asking the user, which it waits for.
func (p *process) requestPermission(ctx context.Context, r acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
	title := ""
	if r.ToolCall.Title != nil {
		title = *r.ToolCall.Title
	}
	name, in := toolName(r.ToolCall.Meta, title), toolInput(r.ToolCall.RawInput)
	p.mu.Lock()
	mode := p.mode
	p.mu.Unlock()
	verdict := "allow"
	if !hearthTool(name) {
		tool, path := gate(name, r.ToolCall.Kind, in, r.ToolCall.Locations)
		verdict = core.Permit(mode, tool, path, p.cwd)
	}
	switch verdict {
	case "allow":
		o, ok := selectOption(r.Options, allowKinds)
		if !ok {
			return acpsdk.RequestPermissionResponse{}, fmt.Errorf("permission request for %s offers no allow option", name)
		}
		return acpsdk.RequestPermissionResponse{Outcome: o}, nil
	case "deny":
		// The agent gets no word of why: a selected outcome carries only the option.
		o, _ := selectOption(r.Options, rejectKinds)
		return acpsdk.RequestPermissionResponse{Outcome: o}, nil
	}
	perm := &core.Perm{RequestID: "perm_" + random.Hex(8), Tool: name, Input: in}
	if verdict == "classify" { // there is no classifier for an ACP agent's calls, so the user decides
		perm.Reason = "auto mode can't judge this agent's tool calls"
	}
	if len(perm.Input) == 0 && title != "" {
		perm.Input = map[string]any{"description": title}
	}
	pp := &pendingPerm{options: r.Options, answer: make(chan acpsdk.RequestPermissionOutcome, 1)}
	p.mu.Lock()
	p.perms[perm.RequestID] = pp
	p.mu.Unlock()
	p.emit(core.PermissionRequested{Perm: perm})
	select {
	case o := <-pp.answer:
		return acpsdk.RequestPermissionResponse{Outcome: o}, nil
	case <-ctx.Done(): // the agent withdrew the request
	case <-p.conn.Done():
	}
	if p.takePerm(perm.RequestID) != nil {
		p.emit(core.PermissionCancelled{RequestID: perm.RequestID})
	}
	return acpsdk.RequestPermissionResponse{Outcome: acpsdk.NewRequestPermissionOutcomeCancelled()}, nil
}

// takePerm removes a pending permission request; nil when it was answered or cancelled already.
func (p *process) takePerm(id string) *pendingPerm {
	p.mu.Lock()
	defer p.mu.Unlock()
	pp := p.perms[id]
	delete(p.perms, id)
	return pp
}

// cancelPerms answers every pending permission request as cancelled, as ACP requires when its turn is cancelled.
func (p *process) cancelPerms() {
	p.mu.Lock()
	pending := p.perms
	p.perms = map[string]*pendingPerm{}
	p.mu.Unlock()
	for id, pp := range pending {
		pp.answer <- acpsdk.NewRequestPermissionOutcomeCancelled()
		p.emit(core.PermissionCancelled{RequestID: id})
	}
}

// Allow selects the agent's allow option. ACP can't carry input the user edited, so input is dropped,
// and an "always" answer is a one-time allow: permissions are ignored too.
func (p *process) Allow(requestID string, _ map[string]any, _ []any) error {
	pp := p.takePerm(requestID)
	if pp == nil {
		return errors.New("already answered")
	}
	o, ok := selectOption(pp.options, allowKinds)
	pp.answer <- o
	if !ok {
		return errors.New("the agent offered no allow option, so the request was cancelled")
	}
	return nil
}

// Deny selects the agent's reject option. The message can't reach the agent: ACP's answer is an option id.
func (p *process) Deny(requestID, _ string) error {
	pp := p.takePerm(requestID)
	if pp == nil {
		return errors.New("already answered")
	}
	o, _ := selectOption(pp.options, rejectKinds)
	pp.answer <- o
	return nil
}

// mcpServers is the MCP servers a session starts with: hearth's, when the agent speaks MCP-over-ACP.
func (p *process) mcpServers(init acpsdk.InitializeResponse) []acpsdk.McpServer {
	if !init.AgentCapabilities.McpCapabilities.Acp {
		return []acpsdk.McpServer{}
	}
	id := "hearth-" + random.Hex(16)
	p.mu.Lock()
	p.mcpID = id
	p.mu.Unlock()
	return []acpsdk.McpServer{{Acp: &acpsdk.McpServerAcpInline{Type: "acp", Name: mcpServerName, Id: acpsdk.McpServerAcpId(id)}}}
}

func (p *process) mcpConnect(r acpsdk.UnstableConnectMcpRequest) (acpsdk.UnstableConnectMcpResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mcpID == "" || string(r.AcpId) != p.mcpID {
		return acpsdk.UnstableConnectMcpResponse{}, acpsdk.NewInvalidParams(map[string]any{"error": fmt.Sprintf("unknown MCP server %q", r.AcpId)})
	}
	id := acpsdk.UnstableMcpConnectionId("mcp_" + random.Hex(8))
	p.mcpConns[id] = struct{}{}
	return acpsdk.UnstableConnectMcpResponse{ConnectionId: id}, nil
}

func (p *process) mcpDisconnect(r acpsdk.UnstableDisconnectMcpRequest) (acpsdk.UnstableDisconnectMcpResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.mcpConns[r.ConnectionId]; !ok {
		return acpsdk.UnstableDisconnectMcpResponse{}, unknownConnection(r.ConnectionId)
	}
	delete(p.mcpConns, r.ConnectionId)
	return acpsdk.UnstableDisconnectMcpResponse{}, nil
}

func unknownConnection(id acpsdk.UnstableMcpConnectionId) error {
	return acpsdk.NewInvalidParams(map[string]any{"error": fmt.Sprintf("unknown MCP connection %q", id)})
}

// mcpMessage serves one MCP request over a connection with core's MCP code, the same that serves Claude.
// The ACP result is the MCP result; an MCP error is the ACP error.
func (p *process) mcpMessage(ctx context.Context, r acpsdk.UnstableMessageMcpRequest) (any, error) {
	p.mu.Lock()
	_, ok := p.mcpConns[r.ConnectionId]
	p.mu.Unlock()
	if !ok {
		return nil, unknownConnection(r.ConnectionId)
	}
	if strings.HasPrefix(r.Method, "notifications/") { // MCP notifications get no answer
		return nil, nil
	}
	answer := make(chan map[string]any, 1)
	ev := core.ServeMCP(map[string]any{"id": 0, "method": r.Method, "params": r.Params}, p.tools, mcpServerName, func(resp map[string]any) {
		select {
		case answer <- resp:
		default:
		}
	})
	if ev != nil {
		p.emit(ev)
	}
	select {
	case resp := <-answer:
		if e, ok := resp["error"].(map[string]any); ok {
			code, _ := e["code"].(int)
			msg, _ := e["message"].(string)
			return nil, &acpsdk.RequestError{Code: code, Message: msg}
		}
		return resp["result"], nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.conn.Done():
		return nil, errors.New("the agent is gone")
	}
}
