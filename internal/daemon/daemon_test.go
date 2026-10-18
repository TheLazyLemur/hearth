package daemon_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"hearth/internal/core"
	"hearth/internal/daemon"
	"hearth/internal/testfakes"
)

// drain applies the daemon's changes to c until cond holds of its copy, failing the test when
// they never arrive.
func drain(t *testing.T, c *daemon.Client, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		c.Drain()
		if cond() {
			return
		}
	}
	t.Fatal("the daemon's changes never reached the client")
}

// ended reports whether the daemon stopped serving within d.
func ended(h *testfakes.Harness, d time.Duration) bool {
	select {
	case <-h.Served():
		return true
	case <-time.After(d):
		return false
	}
}

func TestTwoClientsSeeTheSameChange(t *testing.T) {
	// given
	// ... two instances attached to one daemon, over one session
	s := &core.Session{SessionID: "local_1", Title: "render", OriginCwd: "/p", TitleSource: "user"}
	h := testfakes.NewHarness(t, time.Hour, s)
	a, b := h.Dial(t), h.Dial(t)

	// when
	// ... one of them renames the session
	a.Rename(s, "render markdown")

	// then
	// ... both copies show the new title, and the daemon stored it once
	drain(t, b, func() bool { return b.Find(s.SessionID).Title == "render markdown" })
	if got := a.Find(s.SessionID).Title; got != "render markdown" || h.Store.Saved[s.SessionID].Title != "render markdown" {
		t.Fatalf("title %q, saved %+v", got, h.Store.Saved[s.SessionID])
	}
}

func TestFirstPermissionAnswerWins(t *testing.T) {
	// given
	// ... two instances, a turn running, and the agent asking to write a file
	s := &core.Session{SessionID: "local_1", Title: "render", OriginCwd: "/p", TitleSource: "user"}
	h := testfakes.NewHarness(t, time.Hour, s)
	a, b := h.Dial(t), h.Dial(t)
	if err := a.Send(s, "write it"); err != nil {
		t.Fatal(err)
	}
	ag := h.Launcher.Agents[0]
	ag.Tell(s, core.PermissionRequested{Perm: &core.Perm{RequestID: "r1", Tool: "Write"}})
	drain(t, b, func() bool { return len(b.L(s.SessionID).Perms) == 1 })

	// when
	// ... both of them answer it
	first := a.AnswerPerm(s, true, false)
	second := b.AnswerPerm(s, true, false)

	// then
	// ... the agent was told once, with one request id, and both copies show the card gone
	drain(t, a, func() bool { return len(a.L(s.SessionID).Perms) == 0 })
	drain(t, b, func() bool { return len(b.L(s.SessionID).Perms) == 0 })
	if first != nil || second == nil || second.Error() != "already answered" ||
		len(ag.Allowed) != 1 || ag.Allowed[0].RequestID != "r1" {
		t.Fatalf("first %v second %v allowed %+v", first, second, ag.Allowed)
	}
}

func TestBackendCapsReachEveryInstanceAfterASwitch(t *testing.T) {
	// given
	// ... two instances on one session that has not started yet
	s := &core.Session{SessionID: "local_1", Title: "render", OriginCwd: "/p", TitleSource: "user"}
	h := testfakes.NewHarness(t, time.Hour, s)
	a, b := h.Dial(t), h.Dial(t)

	// when
	// ... one of them moves the session to pi, which can neither rewind nor unqueue
	if err := a.SetBackend(s, "pi"); err != nil {
		t.Fatal(err)
	}

	// then
	// ... the instance that did not ask shows pi, with pi's capabilities
	drain(t, b, func() bool { return b.Find(s.SessionID).Backend == "pi" })
	if got := b.Backend(b.Find(s.SessionID)); got.Label != "pi" || got.Rewind || got.Unqueue {
		t.Fatalf("backend %+v", got)
	}
}

func TestInstanceThatStopsReadingIsDroppedNotTheDaemon(t *testing.T) {
	// given
	// ... a peer that says hello and then never reads its socket again
	s := &core.Session{SessionID: "local_1", Title: "render", OriginCwd: "/p", TitleSource: "user"}
	h := testfakes.NewHarness(t, time.Hour, s)
	stuck := h.Conn(t)
	if _, err := fmt.Fprint(stuck, `{"hello":{"version":"1"}}`+"\n"); err != nil {
		t.Fatal(err)
	}

	// when
	// ... it asks more questions than its outbound queue holds, so the daemon lets it go
	for i := 1; i <= 300; i++ {
		if _, err := fmt.Fprintf(stuck, `{"request":{"id":%d,"method":"isRepo","args":{"dir":"/tmp"}}}`+"\n", i); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}

	// then
	// ... the daemon is still there for the instance that keeps up
	b := h.Dial(t)
	b.Rename(s, "render markdown")
	drain(t, b, func() bool { return b.Find(s.SessionID).Title == "render markdown" })
}

func TestDaemonExitsWhenIdleButNotMidTurn(t *testing.T) {
	// given
	// ... a daemon that leaves soon after it goes quiet, with a turn running
	s := &core.Session{SessionID: "local_1", Title: "render", OriginCwd: "/p", TitleSource: "user"}
	h := testfakes.NewHarness(t, 80*time.Millisecond, s)
	c := h.Dial(t)
	if err := c.Send(s, "write it"); err != nil {
		t.Fatal(err)
	}
	ag := h.Launcher.Agents[0]
	ag.Tell(s, core.Consumed{UUID: c.L(s.SessionID).Queued[0].UUID})

	// when
	// ... every instance detaches, leaving the turn to run on
	c.Close()

	// then
	// ... the daemon stays for the agent it still has work for
	if ended(h, 300*time.Millisecond) {
		t.Fatal("the daemon exited mid-turn")
	}

	// when
	// ... the turn ends and the quiet spell passes
	ag.Tell(s, core.TurnEnded{OK: true})

	// then
	// ... nothing is left to serve, so it exits
	if !ended(h, 2*time.Second) {
		t.Fatal("the daemon outlived its last turn and client")
	}
}

func TestVersionMismatchIsRefusedWhileATurnRuns(t *testing.T) {
	// given
	// ... a turn running, and an instance built against another protocol version
	s := &core.Session{SessionID: "local_1", Title: "render", OriginCwd: "/p", TitleSource: "user"}
	h := testfakes.NewHarness(t, 500*time.Millisecond, s)
	c := h.Dial(t)
	if err := c.Send(s, "write it"); err != nil {
		t.Fatal(err)
	}
	ag := h.Launcher.Agents[0]
	ag.Tell(s, core.Consumed{UUID: c.L(s.SessionID).Queued[0].UUID})

	// when
	// ... the other build dials, and then dials again once the turn is over
	busyClient, busyErr := h.DialVersion("0")
	st, statusErr := c.DaemonStatus()
	ag.Tell(s, core.TurnEnded{OK: true})
	drain(t, c, func() bool { return !c.L(s.SessionID).Running })
	c.Close()
	idleClient, idleErr := h.DialVersion("0")

	// then
	// ... both were refused, and the busy one left the daemon and its turn alone; the count
	// ... leaves out the instance asking, which is this one
	var v *daemon.VersionError
	busyRefused, idleRefused := errors.As(busyErr, &v) && v.Busy, errors.As(idleErr, &v) && !v.Busy
	if !busyRefused || busyClient != nil || statusErr != nil || st.Clients != 0 || len(st.Running) != 1 {
		t.Fatalf("refused %v (%v) client %+v status %+v (%v)", busyRefused, busyErr, busyClient, st, statusErr)
	}

	// then
	// ... the idle one found nothing worth keeping the daemon for, so it made way for that build
	if !idleRefused || idleClient != nil || !ended(h, 2*time.Second) {
		t.Fatalf("refused %v (%v) client %+v", idleRefused, idleErr, idleClient)
	}
}

func TestClientCopyMatchesTheDaemonAfterAStreamedTurnAndRewind(t *testing.T) {
	// given
	// ... a session loaded with one turn to rewind to, and an instance that then watches a turn stream
	s := &core.Session{SessionID: "local_1", CLISessionID: "c1", Title: "render", OriginCwd: "/p", TitleSource: "user"}
	h := testfakes.NewHarness(t, time.Hour, s)
	h.Transcripts["c1"] = []core.Entry{{Kind: "user", Text: "first", UUID: "u1"}, {Kind: "assistant", Text: "ok"}}
	c := h.Dial(t)
	c.EnsureLoaded(s)
	drain(t, c, func() bool { return len(c.L(s.SessionID).Entries) == 2 })
	if err := c.Send(s, "next"); err != nil {
		t.Fatal(err)
	}
	ag := h.Launcher.Agents[0]
	ag.Tell(s, core.Consumed{UUID: c.L(s.SessionID).Queued[0].UUID})
	ag.Tell(s, core.TextDelta{Text: "Hel"})
	ag.Tell(s, core.TextDelta{Text: "lo"})
	ag.Tell(s, core.AssistantMessage{Entries: []core.Entry{{Kind: "assistant", Text: "Hello"}}})
	ag.Tell(s, core.TurnEnded{OK: true, Duration: time.Second})

	// when
	// ... the user rewinds to the first turn and the agent confirms
	if err := c.Rewind(s, 1); err != nil {
		t.Fatal(err)
	}
	ag.Tell(s, core.Reply{RequestID: "req-1"})

	// then
	// ... the streaming instance ends on the rewind marker, holding what a fresh instance is
	// ... handed whole
	drain(t, c, func() bool {
		l := c.L(s.SessionID)
		return len(l.Entries) == 6 && l.Entries[5].Text == "— files rewound to before turn 1 —"
	})
	fresh := h.Dial(t)
	if got, want := c.L(s.SessionID).Entries, fresh.L(s.SessionID).Entries; !reflect.DeepEqual(got, want) {
		t.Fatalf("streamed %+v\nsnapshot %+v", got, want)
	}
}

func TestRequestForADeletedSessionIsAnsweredNotFatal(t *testing.T) {
	// given
	// ... two instances on one session, one of which deletes it before the other has drained
	s := &core.Session{SessionID: "local_1", Title: "render", OriginCwd: "/p", TitleSource: "user"}
	h := testfakes.NewHarness(t, time.Hour, s)
	a, b := h.Dial(t), h.Dial(t)
	if err := a.Delete(s); err != nil {
		t.Fatal(err)
	}

	// when
	// ... the other one stars the session it has not heard was deleted
	b.ToggleStar(s)

	// then
	// ... it is told in a notice, and the daemon is still serving
	st, err := b.DaemonStatus()
	if err != nil || st.Clients != 1 || b.Notice() != "no such session" {
		t.Fatalf("status %+v (%v), notice %q", st, err, b.Notice())
	}
}

func TestRewindPreviewAndUnqueueAnswerWhenTheAgentDoes(t *testing.T) {
	// given
	// ... a session with one turn to rewind to and a message queued behind a running agent
	s := &core.Session{SessionID: "local_1", CLISessionID: "c1", Title: "render", OriginCwd: "/p", TitleSource: "user"}
	h := testfakes.NewHarness(t, time.Hour, s)
	h.Transcripts["c1"] = []core.Entry{{Kind: "user", Text: "first", UUID: "u1"}, {Kind: "assistant", Text: "ok"}}
	c := h.Dial(t)
	c.EnsureLoaded(s)
	drain(t, c, func() bool { return len(c.L(s.SessionID).Entries) == 2 })
	if err := c.Send(s, "queued"); err != nil {
		t.Fatal(err)
	}
	ag := h.Launcher.Agents[0]
	var preview core.RewindPreview
	var text string
	previewed := make(chan error)
	unqueued := make(chan error)

	// when
	// ... the agent answers a preview and a cancel, each only after it was asked
	go func() { previewed <- c.PreviewRewind(s, 1, func(r core.RewindPreview) { preview = r }) }()
	<-ag.Asked
	ag.Tell(s, core.Reply{RequestID: "req-1", Rewind: core.RewindPreview{CanRewind: true, Files: 3}})
	previewErr := <-previewed
	go func() { unqueued <- c.Unqueue(s, func(t string) { text = t }) }()
	<-ag.Asked
	ag.Tell(s, core.Reply{RequestID: "req-2", Cancelled: true})
	unqueueErr := <-unqueued

	// then
	// ... the callers get what the agent said, and the queue is already empty in their copy
	if previewErr != nil || unqueueErr != nil || !preview.CanRewind || preview.Files != 3 ||
		text != "queued" || len(c.L(s.SessionID).Queued) != 0 {
		t.Fatalf("preview %+v (%v), unqueued %q (%v), queued %+v", preview, previewErr, text, unqueueErr, c.L(s.SessionID).Queued)
	}
}

func TestInstancesSeeAnAgentStartAndReportToItsParent(t *testing.T) {
	// given
	// ... an instance watching a session whose agent is running a turn
	s := &core.Session{SessionID: "local_1", Title: "render", OriginCwd: "/p", TitleSource: "user"}
	h := testfakes.NewHarness(t, time.Hour, s)
	c := h.Dial(t)
	if err := c.Send(s, "find the callers"); err != nil {
		t.Fatal(err)
	}
	parentAgent := h.Launcher.Agents[0]

	// when
	// ... the session's model starts an agent, and the agent's turn ends
	parentAgent.Tell(s, core.CallTool("start_agent", map[string]any{"title": "Find callers", "prompt": "List callers."}, func(string, bool) {}))
	drain(t, c, func() bool { return len(c.Sessions()) == 2 })
	child := c.Sessions()[0]
	h.Launcher.Agents[1].Tell(child, core.TurnEnded{OK: true})

	// then
	// ... the instance shows the agent's session, and its report queued on the parent
	drain(t, c, func() bool {
		q := c.L(s.SessionID).Queued
		return len(q) == 2 && q[1].ID == child.SessionID
	})
}
