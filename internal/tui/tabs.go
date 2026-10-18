package tui

import (
	"fmt"
	"math"
	"slices"
	"time"

	uv "github.com/charmbracelet/ultraviolet"

	"hearth/internal/core"
)

// tab is an agent shown beside the session the user started. That session is always tab 1 and has no tab of its own.
type tab struct {
	id      string
	closeAt time.Time // zero: not due to close
	seen    bool      // shown while finished: leaving it starts the countdown
	pinned  bool      // the user typed into it: it stays until :tabclose
}

// tabGrace is how long a failed, stopped or just-left agent keeps its tab: Claude Code's subagent panel waits this long.
const tabGrace = 30 * time.Second

// syncTabs opens a tab for each agent that works and closes the tabs whose time is up.
func (u *UI) syncTabs() {
	now, kept := u.now(), map[string][]*tab{} // rebuilt, so a deleted or archived root drops its tabs
	u.agents = u.app.Agents()
	for _, root := range u.app.Visible(false) {
		ts := u.tabs[root.SessionID]
		for _, ag := range u.agents[root.SessionID] {
			if u.working(ag) && tabIndex(ts, ag.SessionID) < 0 {
				ts = append(ts, &tab{id: ag.SessionID})
			}
		}
		if ts = slices.DeleteFunc(ts, func(t *tab) bool { return u.tabDone(t, now) }); len(ts) > 0 {
			kept[root.SessionID] = ts
		}
	}
	u.tabs = kept
}

// tabDone applies Claude Code's rules for its subagent panel (2.1.302) to t.
func (u *UI) tabDone(t *tab, now time.Time) bool {
	s := u.app.Find(t.id)
	switch {
	case s == nil:
		return true // deleted
	case u.cur == t.id:
		t.seen, t.closeAt = !u.working(s), time.Time{}
		return false
	case u.working(s):
		t.seen, t.closeAt = false, time.Time{}
		return false
	case t.pinned:
		return false
	case !t.closeAt.IsZero():
		return !now.Before(t.closeAt)
	}
	l := u.app.L(t.id)
	switch {
	case t.seen:
		t.seen, t.closeAt = false, now.Add(tabGrace)
	case l.Report == "failed" && l.Retryable:
		// kept for a retry
	case l.Report == "failed" || l.Report == "stopped":
		t.closeAt = now.Add(tabGrace)
	default:
		return true // its report reached the parent
	}
	return false
}

func (u *UI) working(s *core.Session) bool {
	st := u.app.Status(s)
	return st == "running" || st == "waiting"
}

// closing reports whether a tab waits to close, so the clock must keep redrawing.
func (u *UI) closing() bool {
	for _, ts := range u.tabs {
		for _, t := range ts {
			if !t.closeAt.IsZero() {
				return true
			}
		}
	}
	return false
}

func tabIndex(ts []*tab, id string) int {
	return slices.IndexFunc(ts, func(t *tab) bool { return t.id == id })
}

// tabIDs lists s's tab line: its root, then the root's agent tabs.
func (u *UI) tabIDs(s *core.Session) []string {
	root := u.app.Root(s)
	ids := []string{root.SessionID}
	for _, t := range u.tabs[root.SessionID] {
		ids = append(ids, t.id)
	}
	return ids
}

// show puts session id in the main pane, as a tab of the session the sidebar selects.
func (u *UI) show(id string) {
	u.cur, u.tv = id, transcriptView{cursor: position{line: math.MaxInt}} // the next draw clamps it to the newest line
	u.syncTabs()                                                          // the one left starts its countdown now
}

// openTab opens ag's tab, or goes to it if it's open.
func (u *UI) openTab(ag *core.Session) {
	root := u.app.Root(ag).SessionID
	if tabIndex(u.tabs[root], ag.SessionID) < 0 {
		u.tabs[root] = append(u.tabs[root], &tab{id: ag.SessionID})
	}
	u.sel = root
	u.show(ag.SessionID)
}

// switchTab goes to tab n (1-based), or with no n to the next or previous tab, wrapping.
func (u *UI) switchTab(s *core.Session, next bool, n int) {
	ids := u.tabIDs(s)
	i := slices.Index(ids, s.SessionID)
	switch {
	case n > len(ids):
		u.app.Notice = fmt.Sprintf("no tab %d", n)
		return
	case n > 0:
		i = n - 1
	case next:
		i = (i + 1) % len(ids)
	default:
		i = (i + len(ids) - 1) % len(ids)
	}
	u.show(ids[i])
}

// tabKey takes a tab count digit; it reports whether it used char.
func (u *UI) tabKey(char rune) bool {
	if char >= '1' && char <= '9' || u.count > 0 && char == '0' {
		u.count = u.count*10 + int(char-'0')
		return true
	}
	return false
}

// closeTab closes s's tab. A working agent keeps its tab: closing one never stops it.
func (u *UI) closeTab(s *core.Session) {
	root := u.app.Root(s)
	switch {
	case root == s:
		u.app.Notice = "this is the session's own tab; :q quits"
	case u.working(s):
		u.app.Notice = "the agent is working: esc stops it, then :tabclose"
	default:
		ts := u.tabs[root.SessionID]
		u.tabs[root.SessionID] = slices.Delete(ts, tabIndex(ts, s.SessionID), tabIndex(ts, s.SessionID)+1)
		u.show(root.SessionID)
	}
}

// agentLineAt is the agent named by the transcript line under the view's cursor, if it is an agent line.
func (u *UI) agentLineAt(s *core.Session) *core.Session {
	if u.tv.cursor.line >= len(u.slines) {
		return nil
	}
	i := u.slines[u.tv.cursor.line].entry - 1
	es := u.app.L(s.SessionID).Entries
	if i < 0 || i >= len(es) || es[i].Kind != "info" || es[i].ID == "" {
		return nil
	}
	return u.app.Find(es[i].ID)
}

// tabLine draws s's tab line, or nil when its root has no agent tabs.
func (u *UI) tabLine(s *core.Session) *line {
	ids := u.tabIDs(s)
	if len(ids) == 1 {
		return nil
	}
	var spans []span
	for i, id := range ids {
		t := u.app.Find(id)
		st := fg(th.inactive)
		if id == s.SessionID {
			st = uv.Style{Fg: th.text, Attrs: uv.AttrBold}
		}
		if i > 0 {
			spans = append(spans, span{" │ ", fg(th.subtle)})
		}
		spans = append(spans, span{fmt.Sprintf("%d ", i+1), st})
		if i > 0 {
			if p := slices.Index(ids, t.SpawnedFrom.SessionID); p > 0 {
				spans = append(spans, span{fmt.Sprintf("↳%d ", p+1), fg(th.subtle)})
			}
			g, gst := u.tabGlyph(t)
			spans = append(spans, span{g + " ", gst})
		}
		spans = append(spans, span{truncateRunes(t.Title, 24), st})
	}
	l := ln(spans...)
	return &l
}

// tabGlyph is an agent's state on the tab line: working, needs you, or how its last turn ended.
func (u *UI) tabGlyph(s *core.Session) (string, uv.Style) {
	switch u.app.Status(s) {
	case "running", "waiting":
		return u.statusGlyph(s)
	}
	switch u.app.L(s.SessionID).Report {
	case "completed":
		return "✓", fg(th.success)
	case "failed":
		return "✗", fg(th.error)
	case "stopped":
		return "■", fg(th.inactive)
	}
	return " ", uv.Style{}
}

// agentsOf counts root's working agents, and says whether one waits on the user, as of the last syncTabs.
func (u *UI) agentsOf(root *core.Session) (working int, waiting bool) {
	for _, ag := range u.agents[root.SessionID] {
		if u.working(ag) {
			working++
		}
		waiting = waiting || u.app.Status(ag) == "waiting"
	}
	return working, waiting
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
