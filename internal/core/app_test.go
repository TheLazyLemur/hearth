package core_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"hearth/internal/core"
	"hearth/internal/testfakes"
)

// testApp builds an app over the shared fakes: claude, which can rewind and unqueue, and
// pi, which can do neither.
func testApp(t *testing.T, loaded ...*core.Session) (*core.App, *testfakes.Fakes) {
	t.Helper()
	return testfakes.App(t, loaded...)
}

// withAgent gives s an agent with no turn running, as a session that has talked before.
func withAgent(a *core.App, s *core.Session) *testfakes.Agent {
	return testfakes.WithAgent(a, s)
}

func TestAlwaysAllowAnswersWithSuggestedPermissions(t *testing.T) {
	// given
	// ... a live session whose agent asks to use Write with an acceptEdits suggestion
	a, _ := testApp(t)
	s := a.NewSession("/tmp", false)
	ag := withAgent(a, s)
	sug := []any{map[string]any{"type": "setMode", "mode": "acceptEdits", "destination": "session"}}
	a.Handle(core.AgentEvent{s.SessionID, core.PermissionRequested{&core.Perm{RequestID: "r1", Tool: "Write", Input: map[string]any{"file_path": "a.txt"}, Suggestions: sug}}})
	waiting := a.Status(s)

	// when
	// ... the user chooses "always"
	err := a.AnswerPerm(s, true, true)

	// then
	// ... the agent gets an allow carrying the input and the suggested permission update
	want := []testfakes.AllowCall{{RequestID: "r1", Input: map[string]any{"file_path": "a.txt"}, Permissions: sug}}
	if err != nil || waiting != "waiting" || !reflect.DeepEqual(ag.Allowed, want) || a.Status(s) != "idle" {
		t.Fatalf("err %v waiting %q allowed %+v", err, waiting, ag.Allowed)
	}
}

func TestAcceptedTaskSuggestionStartsSessionInWorktree(t *testing.T) {
	// given
	// ... a session in a git repo whose model suggested a task
	a, f := testApp(t)
	f.Worktrees.Repo = true
	parent := a.NewSession("/repo", false)
	withAgent(a, parent)
	var replyErr bool
	a.Handle(core.AgentEvent{parent.SessionID, core.TaskSuggested{Chip: core.Chip{Title: "Fix stale docs", Prompt: "Update README.md", Tldr: "x"},
		Reply: func(_ string, isErr bool) { replyErr = isErr }}})

	// when
	// ... the user accepts the chip
	s, err := a.AcceptChip(parent, 0)

	// then
	// ... a new linked session runs in a fresh worktree and its agent receives the task prompt
	if err != nil || replyErr || s.WorktreePath != "/repo/.claude/worktrees/wt" || s.Cwd != s.WorktreePath || s.Branch != "claude/wt" ||
		s.SpawnedFrom.SessionID != parent.SessionID || s.Title != "Fix stale docs" || len(a.L(parent.SessionID).Chips) != 0 ||
		len(f.Launcher.Agents) != 2 || !reflect.DeepEqual(f.Launcher.Agents[1].Sent, []string{"Update README.md"}) {
		t.Fatalf("err %v session %+v agents %+v", err, s, f.Launcher.Agents)
	}
}

func TestDeleteKeepsSessionWhenWorktreeCannotBeRemoved(t *testing.T) {
	// given
	// ... a session in a worktree that refuses removal, as git does with uncommitted changes
	a, f := testApp(t)
	f.Worktrees.Repo, f.Worktrees.RemoveErr = true, errors.New("contains modified or untracked files")
	s := a.NewSession("/repo", true)
	withAgent(a, s)
	a.Send(s, "dirty work")

	// when
	// ... the session is deleted
	err := a.Delete(s)

	// then
	// ... deletion fails, and the session record remains
	if _, saved := f.Store.Saved[s.SessionID]; err == nil || !saved || a.Find(s.SessionID) == nil {
		t.Fatalf("err %v saved %v", err, saved)
	}
}

func TestQueuedMessageJoinsTranscriptWhenConsumedOrComesBackWhenCancelled(t *testing.T) {
	// given
	// ... a session mid-turn with two messages sent while Claude is working
	a, _ := testApp(t)
	s := a.NewSession("/tmp", false)
	ag := withAgent(a, s)
	a.Send(s, "also do X")
	a.Send(s, "and Y")
	l := a.L(s.SessionID)
	first, second := l.Queued[0], l.Queued[1]
	var pulled string
	a.Unqueue(s, func(text string) { pulled = text })

	// when
	// ... the agent consumes the first and confirms cancelling the second
	a.Handle(core.AgentEvent{s.SessionID, core.Consumed{UUID: first.UUID}})
	a.Handle(core.AgentEvent{s.SessionID, core.Reply{RequestID: "req-1", Cancelled: true}})

	// then
	// ... the first is a rewindable transcript turn, the second is back in the user's hands, nothing stays queued
	if !reflect.DeepEqual(ag.Requests, []string{"cancel " + second.UUID}) {
		t.Fatalf("requests %+v", ag.Requests)
	}
	if len(l.Queued) != 0 || pulled != "and Y" || len(core.RewindTargets(l)) != 1 || core.RewindTargets(l)[0].UUID != first.UUID {
		t.Fatalf("queued %+v pulled %q entries %+v", l.Queued, pulled, l.Entries)
	}
}

func TestSessionRunsOnTheBackendChosenBeforeItStarts(t *testing.T) {
	// given
	// ... a new session switched to pi before its first message
	a, f := testApp(t)
	s := a.NewSession("/tmp", false)
	chosen := a.SetBackend(s, "pi")

	// when
	// ... the first message starts it, then the user tries to switch back
	sendErr := a.Send(s, "hi")
	switchErr := a.SetBackend(s, core.DefaultBackend)

	// then
	// ... pi ran it, and the backend stays pi once started
	if chosen != nil || sendErr != nil || switchErr == nil || s.Backend != "pi" || f.Store.Saved[s.SessionID].Backend != "pi" ||
		len(f.PiLauncher.Agents) != 1 || len(f.Launcher.Agents) != 0 {
		t.Fatalf("chosen %v send %v switch %v session %+v", chosen, sendErr, switchErr, s)
	}
}

func TestBackendRefusesFeaturesItLacks(t *testing.T) {
	// given
	// ... a running pi session with a queued message
	a, _ := testApp(t)
	s := a.NewSession("/tmp", false)
	a.SetBackend(s, "pi")
	ag := withAgent(a, s)
	a.Send(s, "hi")

	// when
	// ... the user rewinds and pulls the message back
	rewindErr := a.PreviewRewind(s, 1, func(core.RewindPreview) {})
	unqueueErr := a.Unqueue(s, func(string) {})

	// then
	// ... each is refused before reaching the agent
	if rewindErr == nil || unqueueErr == nil || len(ag.Requests) != 0 {
		t.Fatalf("rewind %v unqueue %v requests %v", rewindErr, unqueueErr, ag.Requests)
	}
}

func TestSessionsSavedBeforeBackendsRunOnTheDefault(t *testing.T) {
	// given
	// ... a stored session with no backend recorded
	old := &core.Session{SessionID: "local_old", Cwd: "/tmp", PermissionMode: "default"}

	// when
	// ... the app loads it and the user messages it
	a, f := testApp(t, old)
	err := a.Send(old, "hi")

	// then
	// ... it runs on the default backend
	if err != nil || old.Backend != core.DefaultBackend || a.Backend(old).Label != "Claude Code" || len(f.Launcher.Agents) != 1 {
		t.Fatalf("err %v session %+v", err, old)
	}
}

func TestModelsAreListedOncePerBackendInTheBackground(t *testing.T) {
	// given
	// ... two pi sessions and a pi that offers one model
	a, f := testApp(t)
	f.PiLauncher.Offers = []core.Model{{ID: "ollama/deepseek-v4.1-flash:cloud", Name: "DeepSeek V4.1 Flash", Efforts: []string{"off", "high"}}}
	s1, s2 := a.NewSession("/tmp", false), a.NewSession("/tmp", false)
	a.SetBackend(s1, "pi")
	a.SetBackend(s2, "pi")

	// when
	// ... the first session asks for models, the listing arrives, and the second asks too
	_, loading := a.Models(s1)
	a.Handle(<-a.Events())
	ms, stillLoading := a.Models(s2)

	// then
	// ... the first ask is loading, the second gets the cached list without listing again
	if !loading || stillLoading || !reflect.DeepEqual(ms, f.PiLauncher.Offers) || f.PiLauncher.Lists != 1 || f.Launcher.Lists != 0 {
		t.Fatalf("loading %v %v models %+v lists %d", loading, stillLoading, ms, f.PiLauncher.Lists)
	}
}

func TestEffortsFollowTheSessionsModel(t *testing.T) {
	// given
	// ... a pi session on a listed model
	a, f := testApp(t)
	f.PiLauncher.Offers = []core.Model{{ID: "ollama/a", Efforts: []string{"off", "high"}}, {ID: "ollama/b"}}
	s := a.NewSession("/tmp", false)
	a.SetBackend(s, "pi")
	a.Models(s)
	a.Handle(<-a.Events())

	// when
	// ... the session picks model a
	a.SetModel(s, "ollama/a")

	// then
	// ... its effort levels are model a's
	if got := a.Efforts(s); !reflect.DeepEqual(got, []string{"off", "high"}) {
		t.Fatalf("efforts %v", got)
	}
}

func TestAgentReportsItsFinalMessageToTheSessionThatStartedIt(t *testing.T) {
	// given
	// ... a pi session in acceptEdits mode whose model starts an agent, which ends its turn with a final message
	a, f := testApp(t)
	parent := a.NewSession("/repo", false)
	a.SetBackend(parent, "pi")
	a.SetMode(parent, "acceptEdits")
	parentAgent := withAgent(a, parent)
	var reply string
	var replyErr bool
	a.Handle(core.AgentEvent{parent.SessionID, core.CallTool("start_agent", map[string]any{"title": "Find callers", "prompt": "List every caller of Send."},
		func(text string, isErr bool) { reply, replyErr = text, isErr })})
	child := a.Sessions()[0]
	a.Handle(core.AgentEvent{child.SessionID, core.AssistantMessage{Entries: []core.Entry{{Kind: "assistant", Text: "Send has 3 callers."}}}})

	// when
	// ... the agent's turn ends, then the user sends the agent a message of their own and that turn ends too
	a.Handle(core.AgentEvent{child.SessionID, core.TurnEnded{OK: true}})
	a.Send(child, "thanks")
	a.Handle(core.AgentEvent{child.SessionID, core.TurnEnded{OK: true}})

	// then
	// ... the agent ran linked to the parent on its backend and mode, and the parent heard back once, with the final message
	childAgent := f.PiLauncher.Agents[1] // after the parent's own
	if replyErr || !strings.Contains(reply, child.SessionID) || child.SpawnedFrom.SessionID != parent.SessionID || child.SpawnedFrom.Kind != "agent" ||
		child.Backend != "pi" || child.PermissionMode != "acceptEdits" || child.Cwd != "/repo" || child.Title != "Find callers" ||
		!strings.Contains(childAgent.Sent[0], "List every caller of Send.") {
		t.Fatalf("reply %q %v child %+v sent %q", reply, replyErr, child, childAgent.Sent)
	}
	if len(parentAgent.Sent) != 1 || !strings.Contains(parentAgent.Sent[0], `status="completed"`) || !strings.Contains(parentAgent.Sent[0], "Send has 3 callers.") {
		t.Fatalf("parent sent %q", parentAgent.Sent)
	}
}

func TestAgentsCannotNestPastTheDepthLimit(t *testing.T) {
	// given
	// ... an agent already three levels below the session the user started
	root := &core.Session{SessionID: "local_root", Cwd: "/repo", PermissionMode: "default"}
	c1 := &core.Session{SessionID: "local_c1", Cwd: "/repo", PermissionMode: "default", SpawnedFrom: &core.SpawnedFrom{SessionID: "local_root", Kind: "agent"}}
	c2 := &core.Session{SessionID: "local_c2", Cwd: "/repo", PermissionMode: "default", SpawnedFrom: &core.SpawnedFrom{SessionID: "local_c1", Kind: "agent"}}
	c3 := &core.Session{SessionID: "local_c3", Cwd: "/repo", PermissionMode: "default", SpawnedFrom: &core.SpawnedFrom{SessionID: "local_c2", Kind: "agent"}}
	a, f := testApp(t, root, c1, c2, c3)
	withAgent(a, c3)
	var reply string
	var replyErr bool

	// when
	// ... it tries to start another agent
	a.Handle(core.AgentEvent{c3.SessionID, core.CallTool("start_agent", map[string]any{"title": "Deeper", "prompt": "Go deeper."},
		func(text string, isErr bool) { reply, replyErr = text, isErr })})

	// then
	// ... it is refused with the limit, and no session starts
	if !replyErr || !strings.Contains(reply, "depth 4 of 3") || len(a.Sessions()) != 4 || len(f.Launcher.Agents) != 1 /* c3's own */ {
		t.Fatalf("reply %q %v sessions %d", reply, replyErr, len(a.Sessions()))
	}
}

func TestParentHearsWhenItsAgentExitsMidTurn(t *testing.T) {
	// given
	// ... a session with a running agent
	a, _ := testApp(t)
	parent := a.NewSession("/repo", false)
	parentAgent := withAgent(a, parent)
	a.Handle(core.AgentEvent{parent.SessionID, core.CallTool("start_agent", map[string]any{"title": "Refactor", "prompt": "Refactor x."}, func(string, bool) {})})
	child := a.Sessions()[0]

	// when
	// ... the agent's process exits before its turn ends
	a.Handle(core.AgentEvent{child.SessionID, core.Exited{Err: errors.New("signal: killed")}})

	// then
	// ... the parent is told the agent failed, and why
	if len(parentAgent.Sent) != 1 || !strings.Contains(parentAgent.Sent[0], `status="failed"`) || !strings.Contains(parentAgent.Sent[0], "signal: killed") {
		t.Fatalf("parent sent %q", parentAgent.Sent)
	}
}

// listed delivers each backend's model list, as hearth's startup listing would.
func listed(a *core.App, claude, pi []core.Model) {
	a.Handle(core.AgentEvent{Event: core.ModelsListed{Backend: core.DefaultBackend, Models: claude}})
	a.Handle(core.AgentEvent{Event: core.ModelsListed{Backend: "pi", Models: pi}})
}

// startAgent calls start_agent for parent and returns its reply.
func startAgent(a *core.App, parent *core.Session, args map[string]any) (reply string, isErr bool) {
	a.Handle(core.AgentEvent{parent.SessionID, core.CallTool("start_agent", args, func(text string, e bool) { reply, isErr = text, e })})
	return reply, isErr
}

func TestStartupListingPutsEveryBackendsModelsInTheAgentTool(t *testing.T) {
	// given
	// ... each backend offers one model
	a, f := testApp(t)
	f.Launcher.Offers = []core.Model{{ID: "opus", Name: "Opus", Efforts: []string{"low", "high"}}}
	f.PiLauncher.Offers = []core.Model{{ID: "ollama/flash", Name: "Flash"}}
	a.ListModels("/repo")
	a.Handle(<-a.Events())
	a.Handle(<-a.Events())

	// when
	// ... a session launches
	a.Send(a.NewSession("/repo", false), "hi")

	// then
	// ... its start_agent description lists both backends' models with their effort levels
	desc := f.Launcher.Tools[2]["description"].(string)
	if !strings.Contains(desc, "claude: opus (Opus), effort low|high") || !strings.Contains(desc, "pi: ollama/flash (Flash), no effort setting") {
		t.Fatalf("description %q", desc)
	}
}

func TestAgentRunsOnTheBackendModelAndEffortItAsksFor(t *testing.T) {
	// given
	// ... a Claude session and a pi model with two effort levels
	a, f := testApp(t)
	listed(a, nil, []core.Model{{ID: "ollama/flash", Efforts: []string{"low", "high"}}})
	parent := a.NewSession("/repo", false)
	withAgent(a, parent)

	// when
	// ... it starts an agent on that pi model at low effort
	reply, isErr := startAgent(a, parent, map[string]any{"title": "Search", "prompt": "Find x.", "backend": "pi", "model": "ollama/flash", "effort": "low"})

	// then
	// ... the agent runs on pi with that model and effort, and the reply says so
	child := a.Sessions()[0]
	if isErr || child.Backend != "pi" || child.Model != "ollama/flash" || child.Effort != "low" || len(f.PiLauncher.Agents) != 1 ||
		!strings.Contains(reply, "on pi, model ollama/flash, effort low") {
		t.Fatalf("reply %q %v child %+v", reply, isErr, child)
	}
}

func TestAgentKeepsTheParentsEffortOnlyWhenItsModelHasIt(t *testing.T) {
	// given
	// ... a session at xhigh effort, and pi models with and without xhigh
	a, _ := testApp(t)
	listed(a, nil, []core.Model{{ID: "ollama/big", Efforts: []string{"high", "xhigh"}}, {ID: "ollama/small", Efforts: []string{"low", "high"}}})
	parent := a.NewSession("/repo", false)
	a.SetEffort(parent, "xhigh")
	withAgent(a, parent)

	// when
	// ... it starts one agent on each model without naming an effort
	startAgent(a, parent, map[string]any{"title": "Big", "prompt": "x", "backend": "pi", "model": "ollama/big"})
	startAgent(a, parent, map[string]any{"title": "Small", "prompt": "x", "backend": "pi", "model": "ollama/small"})

	// then
	// ... the model with xhigh keeps it, and the other uses its default
	big, small := a.Sessions()[1], a.Sessions()[0]
	if big.Effort != "xhigh" || small.Effort != "" {
		t.Fatalf("big %q small %q", big.Effort, small.Effort)
	}
}

func TestAgentOnABackendAloneUsesThatBackendsDefaultModel(t *testing.T) {
	// given
	// ... a Claude session on opus at high effort
	a, _ := testApp(t)
	listed(a, []core.Model{{ID: "opus", Efforts: []string{"high"}}}, nil)
	parent := a.NewSession("/repo", false)
	a.SetModel(parent, "opus")
	a.SetEffort(parent, "high")
	withAgent(a, parent)

	// when
	// ... it starts an agent naming only the pi backend
	startAgent(a, parent, map[string]any{"title": "Search", "prompt": "x", "backend": "pi"})

	// then
	// ... the agent runs on pi's default model, at its default effort
	child := a.Sessions()[0]
	if child.Backend != "pi" || child.Model != "" || child.Effort != "" {
		t.Fatalf("child %+v", child)
	}
}

func TestStartAgentRefusesWhatItCannotHonour(t *testing.T) {
	flash := []core.Model{{ID: "ollama/flash", Efforts: []string{"low", "high"}}}
	cases := []struct {
		name   string
		lists  func(a *core.App)
		args   map[string]any
		reason string
	}{
		{"unknown backend", func(a *core.App) { listed(a, nil, flash) }, map[string]any{"backend": "gemini"}, "unknown backend \"gemini\"; choose one of: claude, pi"},
		{"unknown model", func(a *core.App) { listed(a, nil, flash) }, map[string]any{"backend": "pi", "model": "ollama/nope"}, "pi has no model \"ollama/nope\"; choose one of: ollama/flash"},
		{"unsupported effort", func(a *core.App) { listed(a, nil, flash) }, map[string]any{"backend": "pi", "model": "ollama/flash", "effort": "max"}, "ollama/flash has no effort \"max\"; choose one of: low, high"},
		{"lists loading", func(a *core.App) { a.ListModels("/repo") }, map[string]any{"backend": "pi", "model": "ollama/flash"}, "model lists are still loading"},
		{"listing failed", func(a *core.App) {
			a.Handle(core.AgentEvent{Event: core.ModelsListed{Backend: "pi", Err: errors.New("exec: pi not found")}})
		}, map[string]any{"backend": "pi", "model": "ollama/flash"}, "pi is not available: listing models: exec: pi not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// given
			// ... a session and the model lists of this case
			a, _ := testApp(t)
			c.lists(a)
			parent := a.NewSession("/repo", false)
			withAgent(a, parent)
			c.args["title"], c.args["prompt"] = "Task", "Do x."

			// when
			// ... it asks for an agent hearth cannot honour
			reply, isErr := startAgent(a, parent, c.args)

			// then
			// ... the call is refused with the reason, and no agent starts
			if !isErr || !strings.Contains(reply, c.reason) || len(a.Sessions()) != 1 {
				t.Fatalf("reply %q %v sessions %d", reply, isErr, len(a.Sessions()))
			}
		})
	}
}

func TestAgentsLeaveTheSidebarAndListUnderTheSessionTheUserStarted(t *testing.T) {
	// given
	// ... a session the user started, its agent, that agent's own agent, and an agent whose parent was deleted
	root := &core.Session{SessionID: "local_root", Cwd: "/repo", CreatedAt: 1}
	c1 := &core.Session{SessionID: "local_c1", Cwd: "/repo", CreatedAt: 2, SpawnedFrom: &core.SpawnedFrom{SessionID: "local_root", Kind: "agent"}}
	c2 := &core.Session{SessionID: "local_c2", Cwd: "/repo", CreatedAt: 3, SpawnedFrom: &core.SpawnedFrom{SessionID: "local_c1", Kind: "agent"}}
	orphan := &core.Session{SessionID: "local_orphan", Cwd: "/repo", CreatedAt: 4, SpawnedFrom: &core.SpawnedFrom{SessionID: "local_gone", Kind: "agent"}}
	a, _ := testApp(t, c2, orphan, root, c1)

	// when
	// ... the sidebar's sessions, the nested agent's root and the root's agents are asked for
	visible, top, agents := a.Visible(false), a.Root(c2), a.Agents()["local_root"]

	// then
	// ... the sidebar keeps the root and the orphan, and both agents list under the root, oldest first
	ids := func(ss []*core.Session) (out []string) {
		for _, s := range ss {
			out = append(out, s.SessionID)
		}
		return out
	}
	if !slices.Equal(ids(visible), []string{"local_orphan", "local_root"}) || top != root || !slices.Equal(ids(agents), []string{"local_c1", "local_c2"}) {
		t.Fatalf("visible %v root %s agents %v", ids(visible), top.SessionID, ids(agents))
	}
}

func TestAgentKeepsHowItsLastTurnEnded(t *testing.T) {
	cases := []struct {
		name      string
		work      []core.Event
		end       core.TurnEnded
		report    string
		retryable bool
	}{
		{"finished", []core.Event{core.AssistantMessage{Entries: []core.Entry{{Kind: "assistant", Text: "Done."}}}}, core.TurnEnded{OK: true}, "completed", false},
		{"overloaded after doing work", []core.Event{core.AssistantMessage{Entries: []core.Entry{{Kind: "assistant", Text: "Halfway."}}}},
			core.TurnEnded{Failure: `error_during_execution ["API Error: 529 Overloaded"]`}, "failed", true},
		{"overloaded before doing anything", nil, core.TurnEnded{Failure: `error_during_execution ["API Error: 529 Overloaded"]`}, "failed", false},
		{"out of turns", []core.Event{core.AssistantMessage{Entries: []core.Entry{{Kind: "assistant", Text: "Halfway."}}}},
			core.TurnEnded{Failure: `error_max_turns []`}, "failed", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// given
			// ... a session whose agent did this case's work
			a, _ := testApp(t)
			parent := a.NewSession("/repo", false)
			withAgent(a, parent)
			startAgent(a, parent, map[string]any{"title": "Find callers", "prompt": "List callers."})
			child := a.Sessions()[0]
			for _, e := range c.work {
				a.Handle(core.AgentEvent{child.SessionID, e})
			}

			// when
			// ... the agent's turn ends
			a.Handle(core.AgentEvent{child.SessionID, c.end})

			// then
			// ... the agent keeps how it ended, and the parent shows a line that names the agent
			l, shown := a.L(child.SessionID), a.L(parent.SessionID).Queued
			if l.Report != c.report || l.Retryable != c.retryable || len(shown) != 1 || shown[0].ID != child.SessionID {
				t.Fatalf("report %q retryable %v shown %+v", l.Report, l.Retryable, shown)
			}
		})
	}
}

func TestReloadedReportReadsAsTheAgentLine(t *testing.T) {
	// given
	// ... a report as hearth sends it to a parent, its title quoted
	text := "<agent-report session=\"local_c1\" title=\"Find \\\"Send\\\" callers\" status=\"failed\">\nboom\n</agent-report>\nThis report comes from an agent you started, not from the user."

	// when
	// ... a reloaded transcript reads it, and a message the user typed
	e, ok := core.ReportEntry(text)
	_, typed := core.ReportEntry("<b>not a report</b>")

	// then
	// ... the report becomes the parent's agent line, and the typed message isn't one
	if !ok || typed || e.Kind != "info" || e.ID != "local_c1" || e.Text != `↳ agent "Find \"Send\" callers" failed` {
		t.Fatalf("entry %+v ok %v typed %v", e, ok, typed)
	}
}
