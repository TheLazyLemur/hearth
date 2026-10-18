package app

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"hearth/internal/cli"
	"hearth/internal/core"
	"hearth/internal/daemon"
	"hearth/internal/git"
	"hearth/internal/pi"
	"hearth/internal/store"
	"hearth/internal/tui"
)

// Run is the composition root: `hearth` runs the TUI against the daemon, `hearth daemon` runs
// the daemon itself, which owns the app every instance is a client of.
func Run(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home directory: %w", err)
	}
	dir, err := hearthDir(home)
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "daemon" {
		return runDaemon(args[1:], dir, home)
	}
	launched, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	// the CLI keeps .claude.json beside ~/.claude unless CLAUDE_CONFIG_DIR moves both
	claudeJSON := filepath.Join(cmp.Or(os.Getenv("CLAUDE_CONFIG_DIR"), home), ".claude.json")
	c, err := daemon.Attach(dir)
	if err != nil {
		return err
	}
	if err := tui.Run(c, launched, claudeJSON); errors.Is(err, daemon.ErrGone) {
		return fmt.Errorf("daemon exited, see %s", daemon.LogPath(dir))
	} else if err != nil {
		return err
	}
	return nil
}

// runDaemon serves every instance for this hearth home, or words to the one that does.
func runDaemon(args []string, dir, home string) error {
	word := "serve"
	if len(args) > 0 {
		word = args[0]
	}
	switch word {
	case "serve":
		app, err := newApp(dir, home)
		if err != nil {
			return err
		}
		return daemon.Run(app, dir)
	case "stop", "status":
	default:
		return errors.New("usage: hearth daemon [stop|status]")
	}
	c, err := daemon.Dial(daemon.SocketPath(dir))
	var version *daemon.VersionError
	if errors.As(err, &version) {
		return err // a daemon of another protocol version, and this build can't drive it
	} else if err != nil {
		return fmt.Errorf("no daemon running in %s", dir)
	}
	defer c.Close()
	st, err := c.DaemonStatus()
	if err != nil {
		return err
	}
	head := fmt.Sprintf("daemon v%s · pid %d · started %s",
		st.Version, st.PID, st.Started.Format("2006-01-02 15:04:05"))
	if word == "stop" {
		if err := c.Stop(); err != nil {
			return err
		}
		fmt.Println(head + " · stopped")
		if len(st.Running) == 0 {
			fmt.Println("  nothing was running")
		}
		for _, title := range st.Running {
			fmt.Println("  cut short:", title)
		}
		return nil
	}
	fmt.Printf("%s · %d attached · %d running\n", head, st.Clients, len(st.Running))
	for _, title := range st.Running {
		fmt.Println("  running:", title)
	}
	return nil
}

// hearthDir is where hearth keeps its sessions and the daemon's socket, lock and log.
func hearthDir(home string) (string, error) {
	dir := cmp.Or(os.Getenv("HEARTH_HOME"), filepath.Join(home, ".hearth"))
	// shortcut: one-off move of the pre-rename home; drop once no install still has ~/.claude-tui
	if _, err := os.Stat(dir); os.Getenv("HEARTH_HOME") == "" && errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(filepath.Join(home, ".claude-tui"), dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("move ~/.claude-tui to %s: %w", dir, err)
		}
	}
	return dir, nil
}

// newApp builds the app the daemon owns; instances never touch it, only its protocol.
func newApp(dir, home string) (*core.App, error) {
	claudeConfig := cmp.Or(os.Getenv("CLAUDE_CONFIG_DIR"), filepath.Join(home, ".claude"))
	app, err := core.NewApp(store.Store{Dir: filepath.Join(dir, "sessions")}, map[string]core.Backend{
		core.DefaultBackend: {Label: "Claude Code", Tag: "cl", Rewind: true, Unqueue: true,
			Launcher:    cli.Launcher{Binary: cmp.Or(os.Getenv("HEARTH_CLAUDE_CLI"), "claude")},
			Transcripts: cli.Transcripts{ConfigDir: claudeConfig}},
		"pi": {Label: "pi", Tag: "pi",
			Launcher: pi.Launcher{Binary: "pi"},
			Transcripts: pi.Transcripts{Dir: cmp.Or(os.Getenv("PI_CODING_AGENT_SESSION_DIR"),
				filepath.Join(cmp.Or(os.Getenv("PI_CODING_AGENT_DIR"), filepath.Join(home, ".pi", "agent")), "sessions"))}},
	}, git.Worktrees{})
	if err != nil {
		return nil, fmt.Errorf("load sessions: %w", err)
	}
	return app, nil
}
