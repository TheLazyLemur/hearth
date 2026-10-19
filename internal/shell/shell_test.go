package shell_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"hearth/internal/core"
	"hearth/internal/shell"
)

func TestShellRunsWhatIsTypedAndReportsItsExit(t *testing.T) {
	// given
	// ... a shell opened in a temporary folder
	t.Setenv("SHELL", "/bin/sh")
	out := make(chan core.AgentEvent, 256)
	sh, err := shell.Shells{}.Open("local_1", t.TempDir(), 40, 10, out)
	if err != nil {
		t.Fatal(err)
	}

	// when
	// ... a command is typed into it and entered
	for _, r := range "echo hel''lo" {
		sh.Key(core.Key{Code: r, Text: string(r)})
	}
	sh.Key(core.Key{Code: uv.KeyEnter})

	// then
	// ... a row of the screen shows its output, and the change was reported
	shows := func() bool {
		return slices.ContainsFunc(sh.Screen().Rows, func(r string) bool { return strings.TrimSpace(ansi.Strip(r)) == "hello" })
	}
	for deadline := time.Now().Add(3 * time.Second); !shows() && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if !shows() {
		t.Fatalf("screen %q", sh.Screen().Rows)
	}
	if ev := <-out; ev.SessionID != "local_1" || ev.Event != (core.ShellChanged{}) {
		t.Fatalf("event %+v", ev)
	}

	// when
	// ... it is closed
	sh.Close()

	// then
	// ... it reports its exit, naming itself
	for deadline := time.After(3 * time.Second); ; {
		select {
		case ev := <-out:
			if e, ok := ev.Event.(core.ShellExited); ok {
				if e.Shell != sh {
					t.Fatalf("exited %+v", e)
				}
				return
			}
		case <-deadline:
			t.Fatal("no exit reported")
		}
	}
}
