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
	"hearth/internal/git"
	"hearth/internal/pi"
	"hearth/internal/store"
	"hearth/internal/tui"
)

// Run is the composition root: it builds the adapters, connects them to the core, and runs the TUI.
func Run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home directory: %w", err)
	}
	launched, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	hearthHome := cmp.Or(os.Getenv("HEARTH_HOME"), filepath.Join(home, ".hearth"))
	// shortcut: one-off move of the pre-rename home; drop once no install still has ~/.claude-tui
	if _, err := os.Stat(hearthHome); os.Getenv("HEARTH_HOME") == "" && errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(filepath.Join(home, ".claude-tui"), hearthHome); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("move ~/.claude-tui to %s: %w", hearthHome, err)
		}
	}
	claudeConfig := cmp.Or(os.Getenv("CLAUDE_CONFIG_DIR"), filepath.Join(home, ".claude"))

	app, err := core.NewApp(store.Store{Dir: filepath.Join(hearthHome, "sessions")}, map[string]core.Backend{
		core.DefaultBackend: {Label: "Claude Code", Tag: "cl", Rewind: true, Unqueue: true,
			Launcher:    cli.Launcher{Binary: cmp.Or(os.Getenv("HEARTH_CLAUDE_CLI"), "claude")},
			Transcripts: cli.Transcripts{ConfigDir: claudeConfig}},
		"pi": {Label: "pi", Tag: "pi",
			Launcher: pi.Launcher{Binary: "pi"},
			Transcripts: pi.Transcripts{Dir: cmp.Or(os.Getenv("PI_CODING_AGENT_SESSION_DIR"),
				filepath.Join(cmp.Or(os.Getenv("PI_CODING_AGENT_DIR"), filepath.Join(home, ".pi", "agent")), "sessions"))}},
	}, git.Worktrees{})
	if err != nil {
		return fmt.Errorf("load sessions: %w", err)
	}
	app.ListModels(launched)
	return tui.Run(app, launched)
}
