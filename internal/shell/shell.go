// Package shell runs a session's side-panel shell in a pty, through a terminal emulator
// that keeps its screen, so every client can be shown it.
package shell

import (
	"cmp"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"hearth/internal/core"
)

// frame is the least time between two ShellChanged events of one shell.
const frame = 16 * time.Millisecond

// Shells starts the user's $SHELL.
type Shells struct{}

// term is one shell: its pty, and the emulator reading the pty's output into a screen.
type term struct {
	id      string
	cmd     *exec.Cmd
	pty     *os.File
	out     chan<- core.AgentEvent
	pending atomic.Bool // a ShellChanged is on its way

	mu  sync.Mutex // the emulator is the reader's and the daemon loop's
	emu *vt.Emulator
}

func (Shells) Open(sessionID, cwd string, w, h int, out chan<- core.AgentEvent) (core.Shell, error) {
	cmd := exec.Command(cmp.Or(os.Getenv("SHELL"), "/bin/sh"))
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
	if err != nil {
		return nil, err
	}
	t := &term{id: sessionID, cmd: cmd, pty: f, out: out, emu: vt.NewEmulator(w, h)}
	go t.write()
	go t.read()
	go t.wait()
	return t, nil
}

// write hands the pty what the emulator sends it: keys, pastes, and answers to the shell's
// queries. A queue sits between the two, so the emulator, written to under mu, never waits
// on a shell that is itself waiting for its output to be read.
func (t *term) write() {
	q := make(chan []byte, 1024)
	go func() {
		for b := range q {
			t.pty.Write(b)
		}
	}()
	defer close(q)
	buf := make([]byte, 32*1024)
	for {
		n, err := t.emu.Read(buf)
		if n > 0 {
			q <- slices.Clone(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// read feeds the shell's output to the emulator until the pty closes.
func (t *term) read() {
	buf := make([]byte, 32*1024)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			t.mu.Lock()
			t.emu.Write(buf[:n])
			t.mu.Unlock()
			t.changed()
		}
		if err != nil {
			return
		}
	}
}

// wait closes the pty once the shell ends, and reports it. A background job still holding
// the pty would keep read going, so the shell's own exit is what counts.
func (t *term) wait() {
	t.cmd.Wait()
	t.pty.Close()
	t.mu.Lock()
	t.emu.Close() // ends write
	t.mu.Unlock()
	t.out <- core.AgentEvent{SessionID: t.id, Event: core.ShellExited{Shell: t}}
}

// changed tells the app the screen changed, once per frame however much output arrives.
func (t *term) changed() {
	if t.pending.Swap(true) {
		return
	}
	time.AfterFunc(frame, func() {
		t.pending.Store(false) // before sending: output from here on needs a frame of its own
		t.out <- core.AgentEvent{SessionID: t.id, Event: core.ShellChanged{}}
	})
}

func (t *term) Key(k core.Key) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emu.SendKey(uv.KeyPressEvent{Code: k.Code, Text: k.Text, Mod: uv.KeyMod(k.Mod)})
}

func (t *term) Paste(text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emu.Paste(text)
}

func (t *term) Resize(w, h int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if w == t.emu.Width() && h == t.emu.Height() {
		return
	}
	t.emu.Resize(w, h)
	pty.Setsize(t.pty, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
}

func (t *term) Screen() *core.Screen {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.emu.CursorPosition()
	return &core.Screen{W: t.emu.Width(), H: t.emu.Height(), Rows: strings.Split(t.emu.Render(), "\n"),
		CursorX: p.X, CursorY: p.Y, CursorOn: !t.emu.CursorHidden()}
}

// Close hangs the shell up, as closing a terminal window does; the shell passes it on to its jobs.
func (t *term) Close() { syscall.Kill(-t.cmd.Process.Pid, syscall.SIGHUP) }
