package core

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Limits on start_agent. The first two are Claude Code's defaults for its own subagents.
const (
	maxAgentDepth = 3       // start_agent links below the session the user started
	maxAgents     = 20      // agents one session has working at once
	maxReport     = 100_000 // characters of an agent's final message relayed to its parent
)

// startAgent runs e's prompt as a new session linked to parent. Every turn the user didn't start is
// relayed back to parent as a report when it ends; see report.
func (a *App) startAgent(parent *Session, e AgentRequested) {
	if e.Title == "" || e.Prompt == "" {
		e.Reply("title and prompt are required", true)
		return
	}
	if d := a.agentDepth(parent) + 1; d > maxAgentDepth {
		e.Reply(fmt.Sprintf("Agent nesting limit reached (depth %d of %d). Do this task yourself with your own tools instead of starting another agent.", d, maxAgentDepth), true)
		return
	}
	if n := a.workingAgents(parent); n >= maxAgents {
		e.Reply(fmt.Sprintf("%d agents are already working for this session, the most allowed at once. Don't retry: wait for their reports, or do the task yourself.", n), true)
		return
	}
	backend, model, effort, err := a.agentSetup(parent, e)
	if err != nil {
		e.Reply(err.Error(), true)
		return
	}
	// The agent shares parent's checkout, so it takes parent's folder and branch but no worktree of its own to remove.
	s := a.NewSession(parent.Cwd, false)
	s.OriginCwd, s.Branch = parent.OriginCwd, parent.Branch
	s.Title, s.TitleSource = e.Title, "agent"
	s.Backend, s.Model, s.Effort, s.PermissionMode = backend, model, effort, parent.PermissionMode
	s.SpawnedFrom = &SpawnedFrom{SessionID: parent.SessionID, Title: e.Title, Kind: "agent"}
	a.L(s.SessionID).owesParent = true
	if err := a.Send(s, brief(parent, e.Prompt)); err != nil {
		e.Reply("the agent could not start: "+err.Error(), true)
		a.Delete(s) // it never ran: no transcript or worktree to keep
		return
	}
	e.Reply(fmt.Sprintf("Started agent %s (%q) on %s, model %s, effort %s. It works in the background; its report arrives later as an <agent-report> message.",
		s.SessionID, e.Title, backend, cmp.Or(model, "default"), cmp.Or(effort, "default")), false)
}

// agentSetup is the backend, model and effort an agent of parent runs with: parent's own unless e names
// others. hearth refuses what it can't check, since both backends quietly swap an effort a model lacks.
func (a *App) agentSetup(parent *Session, e AgentRequested) (backend, model, effort string, err error) {
	backend, model = parent.Backend, parent.Model
	inherits := e.Backend == "" && e.Model == ""
	if inherits && e.Effort == "" {
		return backend, model, parent.Effort, nil
	}
	if e.Backend != "" {
		if _, ok := a.backends[e.Backend]; !ok {
			return "", "", "", fmt.Errorf("unknown backend %q; choose one of: %s", e.Backend, strings.Join(slices.Sorted(maps.Keys(a.backends)), ", "))
		}
		backend, model = e.Backend, "" // that backend's default model
	}
	model = cmp.Or(e.Model, model)
	id := model
	if inherits {
		id = cmp.Or(model, a.L(parent.SessionID).Model) // the model parent's agent reports running
	}
	if id == "" {
		if e.Effort != "" {
			return "", "", "", fmt.Errorf("name one of %s's models to set an effort", backend)
		}
		return backend, "", "", nil // the default model, at its default effort
	}
	m, err := a.listedModel(backend, id)
	if err != nil {
		return "", "", "", err
	}
	effort = cmp.Or(e.Effort, parent.Effort)
	if !slices.Contains(m.Efforts, effort) {
		switch {
		case e.Effort != "" && len(m.Efforts) == 0:
			return "", "", "", fmt.Errorf("%s has no effort setting; leave effort out", id)
		case e.Effort != "":
			return "", "", "", fmt.Errorf("%s has no effort %q; choose one of: %s", id, e.Effort, strings.Join(m.Efforts, ", "))
		}
		effort = "" // parent's effort doesn't fit this model: use its default
	}
	return backend, model, effort, nil
}

// listedModel is backend's model id, from the lists ListModels fetched.
func (a *App) listedModel(backend, id string) (Model, error) {
	if err := a.modelErrs[backend]; err != nil {
		return Model{}, fmt.Errorf("%s is not available: listing models: %v", backend, err)
	}
	ms := a.models[backend]
	if ms == nil {
		return Model{}, errors.New("model lists are still loading; leave backend and model out or retry shortly")
	}
	var ids []string
	for _, m := range ms {
		if m.ID == id {
			return m, nil
		}
		ids = append(ids, m.ID)
	}
	return Model{}, fmt.Errorf("%s has no model %q; choose one of: %s", backend, id, strings.Join(ids, ", "))
}

// tools are Tools as a session launching now gets them: start_agent's description lists the models
// listed so far. The description is fixed for the life of the agent process.
func (a *App) tools() []map[string]any {
	var lines []string
	for _, b := range slices.Sorted(maps.Keys(a.backends)) {
		if a.modelErrs[b] != nil {
			continue
		}
		for _, m := range a.models[b] {
			line := b + ": " + m.ID
			if m.Name != "" {
				line += " (" + m.Name + ")"
			}
			if len(m.Efforts) == 0 {
				line += ", no effort setting"
			} else {
				line += ", effort " + strings.Join(m.Efforts, "|")
			}
			lines = append(lines, "- "+line)
		}
	}
	ts := slices.Clone(Tools)
	if len(lines) == 0 {
		return ts
	}
	// shortcut: lists every model uncapped (13 models, ~800 chars today); trim past ~50 models or ~4 kB.
	i := slices.IndexFunc(ts, func(t map[string]any) bool { return t["name"] == "start_agent" })
	ts[i] = maps.Clone(ts[i])
	ts[i]["description"] = ts[i]["description"].(string) +
		"\n\nBackends and models an agent can run on (backend: model (name), effort levels):\n" + strings.Join(lines, "\n")
	return ts
}

// brief is an agent's first message: who started it and how its work gets back, then the task.
func brief(parent *Session, prompt string) string {
	return fmt.Sprintf(`You are an agent, started by the agent working in session %q. It can't see this conversation. When your turn ends, your final message is relayed to it as your report, so put everything it needs there: lead with the outcome, then what you changed or found, with file paths, and anything you could not do or verify. If you are blocked, say so in your final message and stop.

Your task:

%s`, parent.Title, prompt)
}

// temporaryFailure matches turn failures worth retrying as they are: rate limits, overload, server errors, timeouts.
// shortcut: reads the failure text; have each adapter flag these once both backends' real failures are catalogued.
var temporaryFailure = regexp.MustCompile(`(?i)\b(429|5\d\d)\b|rate.?limit|overloaded|timed? ?out|connection reset`)

// report relays the end of agent s's turn to the session that started it.
func (a *App) report(s *Session, l *Live, status, text string) {
	l.owesParent, l.Report, l.Retryable = false, status, false
	parent := a.Find(s.SpawnedFrom.SessionID)
	if parent == nil {
		return // deleted while its agent worked
	}
	if parent.SpawnedFrom != nil && parent.SpawnedFrom.Kind == "agent" {
		a.L(parent.SessionID).owesParent = true // a turn the report starts is relayed up in turn
	}
	msg := fmt.Sprintf("<agent-report session=%q title=%q status=%q>\n%s\n</agent-report>\nThis report comes from an agent you started, not from the user.",
		s.SessionID, s.Title, status, truncate(text, maxReport))
	if err := a.send(parent, msg, agentLine(s.SessionID, s.Title, status)); err != nil {
		a.Notice = fmt.Sprintf("agent %q's report was not delivered: %v", s.Title, err)
	}
}

// agentLine is how a report shows in its parent's transcript: a line naming the agent, which carries its session.
func agentLine(id, title, status string) Entry {
	return Entry{Kind: "info", ID: id, Text: fmt.Sprintf("↳ agent %q %s", title, status)}
}

var reportHeader = regexp.MustCompile(`^<agent-report session="([^"]*)" title="((?:[^"\\]|\\.)*)" status="([^"]*)">`)

// ReportEntry is the agent line a reloaded transcript shows for text, when text is an agent's report.
func ReportEntry(text string) (Entry, bool) {
	m := reportHeader.FindStringSubmatch(text)
	if m == nil {
		return Entry{}, false
	}
	title, err := strconv.Unquote(`"` + m[2] + `"`)
	if err != nil {
		title = m[2]
	}
	return agentLine(m[1], title, m[3]), true
}

const noFinalMessage = "(the agent ended its turn without a final message)"

// finalMessage is the last assistant text of l's current turn.
func finalMessage(l *Live) string {
	for i := len(l.Entries) - 1; i >= 0 && l.Entries[i].Kind != "user"; i-- {
		if l.Entries[i].Kind == "assistant" {
			return l.Entries[i].Text
		}
	}
	return noFinalMessage
}

// Root is the session the user started that s works under: s itself unless s is an agent.
func (a *App) Root(s *Session) *Session {
	for s.SpawnedFrom != nil && s.SpawnedFrom.Kind == "agent" {
		p := a.Find(s.SpawnedFrom.SessionID)
		if p == nil {
			return s // its parent was deleted: it stands alone
		}
		s = p
	}
	return s
}

// Agents lists the agents under each root at any depth, oldest first, by the root's session ID.
func (a *App) Agents() map[string][]*Session {
	out := map[string][]*Session{}
	for _, s := range a.sessions {
		if root := a.Root(s); root != s {
			out[root.SessionID] = append(out[root.SessionID], s)
		}
	}
	for _, ss := range out {
		slices.SortStableFunc(ss, func(x, y *Session) int { return cmp.Compare(x.CreatedAt, y.CreatedAt) })
	}
	return out
}

// agentDepth counts the start_agent links above s.
func (a *App) agentDepth(s *Session) int {
	d := 0
	for s != nil && s.SpawnedFrom != nil && s.SpawnedFrom.Kind == "agent" {
		d++
		s = a.Find(s.SpawnedFrom.SessionID)
	}
	return d
}

// workingAgents counts parent's agents whose report is still to come.
func (a *App) workingAgents(parent *Session) int {
	n := 0
	for _, s := range a.sessions {
		if l := a.live[s.SessionID]; l != nil && l.owesParent && s.SpawnedFrom.SessionID == parent.SessionID {
			n++
		}
	}
	return n
}
