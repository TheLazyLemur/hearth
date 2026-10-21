package acp

import (
	"context"
	"errors"
	"fmt"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"hearth/internal/core"
	"hearth/pkg/random"
)

// configOption is the agent's select option of category cat, or the one whose id is the category's name
// (model, thought_level) when the agent names no categories. It is nil when the agent has none.
func configOption(opts []acpsdk.SessionConfigOption, cat acpsdk.SessionConfigOptionCategory) *acpsdk.SessionConfigOptionSelect {
	for _, o := range opts {
		if o.Select != nil && o.Select.Category != nil && *o.Select.Category == cat {
			return o.Select
		}
	}
	for _, o := range opts {
		if o.Select != nil && string(o.Select.Id) == string(cat) {
			return o.Select
		}
	}
	return nil
}

// choices is o's values, groups flattened.
func choices(o *acpsdk.SessionConfigOptionSelect) []acpsdk.SessionConfigSelectOption {
	var out []acpsdk.SessionConfigSelectOption
	if g := o.Options.Ungrouped; g != nil {
		out = append(out, *g...)
	}
	if g := o.Options.Grouped; g != nil {
		for _, grp := range *g {
			out = append(out, grp.Options...)
		}
	}
	return out
}

// efforts is the thought_level values the agent offers now, none when it has no such option.
func (p *process) efforts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	o := configOption(p.opts, acpsdk.SessionConfigOptionCategoryThoughtLevel)
	if o == nil {
		return nil
	}
	var out []string
	for _, c := range choices(o) {
		out = append(out, string(c.Value))
	}
	return out
}

// currentModel is the model the agent runs, empty when it exposes no model option.
func (p *process) currentModel() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if o := configOption(p.opts, acpsdk.SessionConfigOptionCategoryModel); o != nil {
		return string(o.CurrentValue)
	}
	return ""
}

// setConfig sets the option of category cat to value through session/set_config_option and keeps the
// full option list the agent answers with. what names the change in the error for an agent with no such option.
// Changes go out one at a time: a model change alters the effort options.
func (p *process) setConfig(ctx context.Context, cat acpsdk.SessionConfigOptionCategory, what, value string) error {
	p.cfg.Lock()
	defer p.cfg.Unlock()
	p.mu.Lock()
	o := configOption(p.opts, cat)
	p.mu.Unlock()
	if o == nil {
		return errors.New("this agent does not support " + what + " yet")
	}
	if string(o.CurrentValue) == value {
		return nil
	}
	resp, err := p.conn.SetConfigOption(ctx, acpsdk.SetSessionConfigOptionRequest{ValueId: &acpsdk.SetSessionConfigOptionValueId{
		SessionId: p.acpSID, ConfigId: o.Id, Value: acpsdk.SessionConfigValueId(value)}})
	if err != nil {
		return errors.New(failureText(err))
	}
	if len(resp.ConfigOptions) > 0 {
		p.mu.Lock()
		p.opts = resp.ConfigOptions
		p.mu.Unlock()
	}
	return nil
}

// applySession moves the agent to the model and effort the session records, and returns a notice for
// each change it refused; the agent then runs on what it had.
func (p *process) applySession(ctx context.Context, s *core.Session) (notices []string) {
	if s.Model != "" {
		if err := p.setConfig(ctx, acpsdk.SessionConfigOptionCategoryModel, "switching models", s.Model); err != nil {
			notices = append(notices, fmt.Sprintf("model %q was not applied: %s; the agent stays on its current model", s.Model, err))
		}
	}
	if s.Effort != "" {
		if err := p.setConfig(ctx, acpsdk.SessionConfigOptionCategoryThoughtLevel, "setting an effort", s.Effort); err != nil {
			notices = append(notices, fmt.Sprintf("effort %q was not applied: %s; the agent keeps its current effort", s.Effort, err))
		}
	}
	return notices
}

// setOption answers a SetModel or SetEffort: the change goes out once the handshake is over, and a Reply says how it went.
func (p *process) setOption(cat acpsdk.SessionConfigOptionCategory, what, value string) string {
	id := random.Hex(8)
	go func() {
		<-p.ready
		r := core.Reply{RequestID: id}
		if p.acpSID == "" {
			r.Err = "the agent did not start"
		} else if err := p.setConfig(context.Background(), cat, what, value); err != nil {
			r.Err = err.Error()
		}
		p.emit(r)
	}()
	return id
}

func (p *process) SetModel(model string) string {
	return p.setOption(acpsdk.SessionConfigOptionCategoryModel, "switching models", model)
}

func (p *process) SetEffort(effort string) string {
	return p.setOption(acpsdk.SessionConfigOptionCategoryThoughtLevel, "setting an effort", effort)
}

// Models starts a throwaway agent and reads its model option from a new session. A model choice that
// carries _meta.efforts (lowest first; empty means none) gives its efforts. Otherwise they are read by
// switching to the model, because ACP lists the efforts of the current model only.
// A model the agent refuses to switch to is listed without efforts. An agent with no model option lists none.
// The throwaway session is closed when the agent advertises session/close, and otherwise the agent keeps it.
func (l Launcher) Models(cwd string) ([]core.Model, error) {
	tp, err := l.open(cwd)
	if err != nil {
		return nil, err
	}
	p := newProcess(tp)
	p.replay = &replay{} // any update the agent streams has nowhere to go
	go func() {          // reaps the process
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
	sess, err := p.conn.NewSession(ctx, acpsdk.NewSessionRequest{Cwd: cwd, McpServers: []acpsdk.McpServer{}})
	if err != nil {
		return nil, fmt.Errorf("session/new: %s", failureText(err))
	}
	if sess.SessionId == "" {
		return nil, errors.New("session/new: the agent returned no sessionId")
	}
	p.acpSID, p.opts = sess.SessionId, sess.ConfigOptions
	if init.AgentCapabilities.SessionCapabilities.Close != nil {
		defer p.conn.CloseSession(ctx, acpsdk.CloseSessionRequest{SessionId: sess.SessionId})
	}
	o := configOption(sess.ConfigOptions, acpsdk.SessionConfigOptionCategoryModel)
	if o == nil {
		return nil, nil
	}
	var out []core.Model
	for _, c := range choices(o) {
		m := core.Model{ID: string(c.Value), Name: c.Name}
		if c.Description != nil {
			m.Description = *c.Description
		}
		if listed, ok := c.Meta["efforts"].([]any); ok {
			for _, e := range listed {
				m.Efforts = append(m.Efforts, fmt.Sprint(e))
			}
		} else if err := p.setConfig(ctx, acpsdk.SessionConfigOptionCategoryModel, "switching models", m.ID); err == nil {
			m.Efforts = p.efforts()
		}
		out = append(out, m)
	}
	return out, nil
}
