package tui

import (
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"

	"hearth/internal/core"
)

// tabbed is a UI on a session the user started (local_root) with agents under it; the clock reads *now.
func tabbed(t *testing.T, now *time.Time, agents ...string) (*UI, *core.App) {
	t.Helper()
	ss := []*core.Session{{SessionID: "local_root", Title: "main", OriginCwd: "/p"}}
	for i, id := range agents {
		ss = append(ss, &core.Session{SessionID: id, Title: id, OriginCwd: "/p", CreatedAt: int64(i + 1),
			SpawnedFrom: &core.SpawnedFrom{SessionID: "local_root", Kind: "agent"}})
	}
	app, err := core.NewApp(fakeStore{ss}, map[string]core.Backend{core.DefaultBackend: {Label: "Claude Code", Transcripts: fakeTranscripts{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := &UI{app: app, collapsed: map[string]bool{}, tabs: map[string][]*tab{}, now: func() time.Time { return *now }, cur: "local_root", pane: paneSession, view: 5}
	return u, app
}

func keyPress(r rune) uv.KeyPressEvent { return uv.KeyPressEvent{Code: r, Text: string(r)} }

func TestAgentTabClosesByHowItsTurnEnded(t *testing.T) {
	cases := []struct {
		name           string
		report         string
		retryable      bool
		before         func(u *UI) // after the agent finishes, before the clock is read
		openNow, later bool        // tab open at once, and 31 seconds on
	}{
		{"completed", "completed", false, func(*UI) {}, false, false},
		{"failed", "failed", false, func(*UI) {}, true, false},
		{"stopped", "stopped", false, func(*UI) {}, true, false},
		{"failed for a temporary reason after doing work", "failed", true, func(*UI) {}, true, true},
		{"being viewed", "completed", false, func(u *UI) { u.cur = "local_a1" }, true, true},
		{"left after viewing it finished", "completed", false, func(u *UI) { u.cur = "local_a1"; u.syncTabs(); u.cur = "local_root" }, true, false},
		{"typed into", "completed", false, func(u *UI) { u.tabs["local_root"][0].pinned = true }, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// given
			// ... an agent whose tab opened while it worked, which then finished as in this case
			now := time.Unix(1000, 0)
			u, app := tabbed(t, &now, "local_a1")
			app.L("local_a1").Running = true
			u.syncTabs()
			l := app.L("local_a1")
			l.Running, l.Report, l.Retryable = false, c.report, c.retryable
			c.before(u)

			// when
			// ... the tabs are checked at once, and again 31 seconds later
			u.syncTabs()
			openNow := len(u.tabs["local_root"]) == 1
			now = now.Add(31 * time.Second)
			u.syncTabs()
			later := len(u.tabs["local_root"]) == 1

			// then
			// ... the tab is open or closed as Claude Code's subagent panel would be
			if openNow != c.openNow || later != c.later {
				t.Fatalf("open now %v, 31s later %v", openNow, later)
			}
		})
	}
}

func TestTabKeysMoveBetweenTabsAndCloseOnlyFinishedAgents(t *testing.T) {
	// given
	// ... a session viewed in its transcript, with two working agents
	now := time.Unix(1000, 0)
	u, app := tabbed(t, &now, "local_a1", "local_a2")
	app.L("local_a1").Running, app.L("local_a2").Running = true, true
	u.syncTabs()
	root := app.Find("local_root")
	u.slines = screenLines(u.transcript(root, app.L("local_root"), 60))
	u.focusView()

	// when
	// ... gt, 3gt and gT move along the tab line, then :tabclose is tried while the agent works and after it finishes
	u.viewKey(root, keyPress('g'))
	u.viewKey(root, keyPress('t'))
	afterGt := u.cur
	u.viewKey(app.Find(u.cur), keyPress('3'))
	u.viewKey(app.Find(u.cur), keyPress('g'))
	u.viewKey(app.Find(u.cur), keyPress('t'))
	after3gt := u.cur
	u.viewKey(app.Find(u.cur), keyPress('g'))
	u.viewKey(app.Find(u.cur), keyPress('T'))
	afterGT := u.cur
	u.command("tabclose")
	refused := u.cur == "local_a1" && len(u.tabs["local_root"]) == 2
	app.L("local_a1").Running, app.L("local_a1").Report = false, "completed"
	u.command("tabclose")

	// then
	// ... the keys reached each tab, the working agent's tab stayed, and the finished one closed back to the session
	if afterGt != "local_a1" || after3gt != "local_a2" || afterGT != "local_a1" || !refused ||
		u.cur != "local_root" || len(u.tabs["local_root"]) != 1 || u.tabs["local_root"][0].id != "local_a2" {
		t.Fatalf("gt %s 3gt %s gT %s refused %v now on %s tabs %d", afterGt, after3gt, afterGT, refused, u.cur, len(u.tabs["local_root"]))
	}
}

func TestGfOnAnAgentLineReopensItsTab(t *testing.T) {
	// given
	// ... a session whose transcript ends with a finished agent's line, the agent's tab long closed, the cursor on that line
	now := time.Unix(1000, 0)
	u, app := tabbed(t, &now, "local_a1")
	root := app.Find("local_root")
	l := app.L("local_root")
	l.Entries = []core.Entry{{Kind: "info", ID: "local_a1", Text: `↳ agent "local_a1" completed`}}
	u.slines = screenLines(u.transcript(root, l, 60))
	u.focusView()

	// when
	// ... gf is typed
	u.viewKey(root, keyPress('g'))
	u.viewKey(root, keyPress('f'))

	// then
	// ... the agent's tab is open and shown
	if u.cur != "local_a1" || len(u.tabs["local_root"]) != 1 {
		t.Fatalf("on %s tabs %d", u.cur, len(u.tabs["local_root"]))
	}
}
