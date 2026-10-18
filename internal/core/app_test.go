package core_test

import (
	"errors"
	"reflect"
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
