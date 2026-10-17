package app

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"

	"claude-code-tui/internal/cli"
	"claude-code-tui/internal/core"
	"claude-code-tui/internal/git"
	"claude-code-tui/internal/store"
	"claude-code-tui/internal/tui"
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
	tuiHome := cmp.Or(os.Getenv("CLAUDE_TUI_HOME"), filepath.Join(home, ".claude-tui"))
	claudeConfig := cmp.Or(os.Getenv("CLAUDE_CONFIG_DIR"), filepath.Join(home, ".claude"))
	// the CLI keeps .claude.json beside ~/.claude unless CLAUDE_CONFIG_DIR moves both
	claudeJSON := filepath.Join(cmp.Or(os.Getenv("CLAUDE_CONFIG_DIR"), home), ".claude.json")

	app, err := core.NewApp(
		store.Store{Dir: filepath.Join(tuiHome, "sessions")},
		cli.Transcripts{ConfigDir: claudeConfig},
		cli.Launcher{Binary: cmp.Or(os.Getenv("CLAUDE_TUI_CLI"), "claude")},
		git.Worktrees{},
	)
	if err != nil {
		return fmt.Errorf("load sessions: %w", err)
	}
	return tui.Run(app, launched, claudeJSON)
}
