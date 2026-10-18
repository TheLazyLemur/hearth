package core

import "fmt"

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
	// The agent shares parent's checkout, so it takes parent's folder and branch but no worktree of its own to remove.
	s := a.NewSession(parent.Cwd, false)
	s.OriginCwd, s.Branch = parent.OriginCwd, parent.Branch
	s.Title, s.TitleSource = e.Title, "agent"
	s.Backend, s.Model, s.Effort, s.PermissionMode = parent.Backend, parent.Model, parent.Effort, parent.PermissionMode
	s.SpawnedFrom = &SpawnedFrom{SessionID: parent.SessionID, Title: e.Title, Kind: "agent"}
	a.L(s.SessionID).owesParent = true
	if err := a.Send(s, brief(parent, e.Prompt)); err != nil {
		e.Reply("the agent could not start: "+err.Error(), true)
		a.Delete(s) // it never ran: no transcript or worktree to keep
		return
	}
	e.Reply(fmt.Sprintf("Started agent %s (%q). It works in the background; its report arrives later as an <agent-report> message.", s.SessionID, e.Title), false)
}

// brief is an agent's first message: who started it and how its work gets back, then the task.
func brief(parent *Session, prompt string) string {
	return fmt.Sprintf(`You are an agent, started by the agent working in session %q. It can't see this conversation. When your turn ends, your final message is relayed to it as your report, so put everything it needs there: lead with the outcome, then what you changed or found, with file paths, and anything you could not do or verify. If you are blocked, say so in your final message and stop.

Your task:

%s`, parent.Title, prompt)
}

// report relays the end of agent s's turn to the session that started it.
func (a *App) report(s *Session, l *Live, status, text string) {
	l.owesParent = false
	parent := a.Find(s.SpawnedFrom.SessionID)
	if parent == nil {
		return // deleted while its agent worked
	}
	if parent.SpawnedFrom != nil && parent.SpawnedFrom.Kind == "agent" {
		a.L(parent.SessionID).owesParent = true // a turn the report starts is relayed up in turn
	}
	msg := fmt.Sprintf("<agent-report session=%q title=%q status=%q>\n%s\n</agent-report>\nThis report comes from an agent you started, not from the user.",
		s.SessionID, s.Title, status, truncate(text, maxReport))
	if err := a.send(parent, msg, Entry{Kind: "info", Text: fmt.Sprintf("↳ agent %q %s", s.Title, status)}); err != nil {
		a.Notice = fmt.Sprintf("agent %q's report was not delivered: %v", s.Title, err)
	}
}

// finalMessage is the last assistant text of l's current turn.
func finalMessage(l *Live) string {
	for i := len(l.Entries) - 1; i >= 0 && l.Entries[i].Kind != "user"; i-- {
		if l.Entries[i].Kind == "assistant" {
			return l.Entries[i].Text
		}
	}
	return "(the agent ended its turn without a final message)"
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
