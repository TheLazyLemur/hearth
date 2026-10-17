package core

import (
	"errors"
	"reflect"
	"testing"
)

func TestAlwaysAllowAnswersWithSuggestedPermissions(t *testing.T) {
	// given
	// ... a live session whose agent asks to use Write with an acceptEdits suggestion
	a, _ := testApp(t)
	s := a.NewSession("/tmp", false)
	ag := withAgent(a, s)
	sug := []any{map[string]any{"type": "setMode", "mode": "acceptEdits", "destination": "session"}}
	a.Handle(AgentEvent{s.SessionID, PermissionRequested{&Perm{RequestID: "r1", Tool: "Write", Input: map[string]any{"file_path": "a.txt"}, Suggestions: sug}}})
	waiting := a.Status(s)

	// when
	// ... the user chooses "always"
	err := a.AnswerPerm(s, true, true)

	// then
	// ... the agent gets an allow carrying the input and the suggested permission update
	want := []allowCall{{"r1", map[string]any{"file_path": "a.txt"}, sug}}
	if err != nil || waiting != "waiting" || !reflect.DeepEqual(ag.allowed, want) || a.Status(s) != "idle" {
		t.Fatalf("err %v waiting %q allowed %+v", err, waiting, ag.allowed)
	}
}

func TestAcceptedTaskSuggestionStartsSessionInWorktree(t *testing.T) {
	// given
	// ... a session in a git repo whose model suggested a task
	a, f := testApp(t)
	f.worktrees.repo = true
	parent := a.NewSession("/repo", false)
	withAgent(a, parent)
	var replyErr bool
	a.Handle(AgentEvent{parent.SessionID, TaskSuggested{Chip: Chip{Title: "Fix stale docs", Prompt: "Update README.md", Tldr: "x"},
		Reply: func(_ string, isErr bool) { replyErr = isErr }}})

	// when
	// ... the user accepts the chip
	s, err := a.AcceptChip(parent, 0)

	// then
	// ... a new linked session runs in a fresh worktree and its agent receives the task prompt
	if err != nil || replyErr || s.WorktreePath != "/repo/.claude/worktrees/wt" || s.Cwd != s.WorktreePath || s.Branch != "claude/wt" ||
		s.SpawnedFrom.SessionID != parent.SessionID || s.Title != "Fix stale docs" || len(a.L(parent.SessionID).Chips) != 0 ||
		len(f.launcher.agents) != 1 || !reflect.DeepEqual(f.launcher.agents[0].sent, []string{"Update README.md"}) {
		t.Fatalf("err %v session %+v agents %+v", err, s, f.launcher.agents)
	}
}

func TestDeleteKeepsSessionWhenWorktreeCannotBeRemoved(t *testing.T) {
	// given
	// ... a session in a worktree that refuses removal, as git does with uncommitted changes
	a, f := testApp(t)
	f.worktrees.repo, f.worktrees.removeErr = true, errors.New("contains modified or untracked files")
	s := a.NewSession("/repo", true)
	withAgent(a, s)
	a.Send(s, "dirty work")

	// when
	// ... the session is deleted
	err := a.Delete(s)

	// then
	// ... deletion fails, and the session record remains
	if _, saved := f.store.saved[s.SessionID]; err == nil || !saved || a.Find(s.SessionID) == nil {
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
	a.Handle(AgentEvent{s.SessionID, Consumed{UUID: first.UUID}})
	a.Handle(AgentEvent{s.SessionID, Reply{RequestID: "req-1", Cancelled: true}})

	// then
	// ... the first is a rewindable transcript turn, the second is back in the user's hands, nothing stays queued
	if !reflect.DeepEqual(ag.requests, []string{"cancel " + second.UUID}) {
		t.Fatalf("requests %+v", ag.requests)
	}
	if len(l.Queued) != 0 || pulled != "and Y" || len(RewindTargets(l)) != 1 || RewindTargets(l)[0].UUID != first.UUID {
		t.Fatalf("queued %+v pulled %q entries %+v", l.Queued, pulled, l.Entries)
	}
}
