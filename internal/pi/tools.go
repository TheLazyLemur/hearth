package pi

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"io"

	"hearth/internal/core"
)

// hostJS runs pi's own CLI entry point with core.Tools added and every other tool call
// gated by the TUI; see host.mjs.
//
//go:embed host.mjs
var hostJS string

// serve turns what host.mjs sends on r into core events: tool calls, and permission
// requests the mode doesn't settle. Answers go back on p.channel.
func (p *process) serve(r io.Reader, emit func(core.Event)) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		var m message
		if len(line) > 0 && json.Unmarshal(line, &m) == nil {
			if ev := p.handle(m); ev != nil {
				emit(ev)
			}
		}
		if err != nil {
			return
		}
	}
}

// message is one line host.mjs sends; see the top of host.mjs.
type message struct {
	Type, ID, Name string
	Arguments      map[string]any
	MustAsk        bool   // the classifier flagged it: open the dialog whatever the mode
	Reason         string // the classifier's reason
}

func (p *process) handle(m message) core.Event {
	switch m.Type {
	case "tool":
		return core.CallTool(m.Name, m.Arguments, func(text string, isErr bool) {
			p.answer(map[string]any{"id": m.ID, "text": text, "isError": isErr})
		})
	case "permission":
		ask := core.PermissionRequested{Perm: &core.Perm{RequestID: m.ID, Tool: m.Name, Input: m.Arguments, Reason: m.Reason}}
		if m.MustAsk {
			return ask
		}
		p.mu.Lock()
		mode := p.mode
		p.mu.Unlock()
		path, _ := m.Arguments["path"].(string)
		switch core.Permit(mode, m.Name, path, p.cwd) {
		case "allow":
			p.answer(map[string]any{"id": m.ID, "allow": true})
		case "deny":
			p.answer(map[string]any{"id": m.ID, "allow": false, "text": "Plan mode allows only read-only tools; the user must switch mode first."})
		case "classify":
			p.answer(map[string]any{"id": m.ID, "classify": true})
		default:
			return ask
		}
	case "cancel":
		return core.PermissionCancelled{RequestID: m.ID}
	}
	return nil
}

// answer writes one line to host.mjs. A failed write means pi exited; its Exited event reports that.
func (p *process) answer(m map[string]any) {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err) // maps of JSON values always marshal
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.channel.Write(append(b, '\n'))
}
