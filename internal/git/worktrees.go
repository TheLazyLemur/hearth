package git

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"hearth/internal/core"
	"hearth/pkg/random"
)

var _ core.Worktrees = Worktrees{}

type Worktrees struct{}

func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (Worktrees) IsRepo(dir string) bool {
	_, err := git(dir, "rev-parse", "--show-toplevel")
	return err == nil
}

// Create mirrors the desktop layout: <repo>/.claude/worktrees/<slug>-<hex>
// on branch claude/<slug>-<hex>, cut from HEAD. The worktree's Cwd is the
// origin's sub-path inside it.
func (Worktrees) Create(originCwd, prompt string) (core.Worktree, error) {
	root, err := git(originCwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return core.Worktree{}, err
	}
	src, _ := git(root, "rev-parse", "--abbrev-ref", "HEAD")
	name := slugify(prompt) + "-" + random.Hex(6)
	path := filepath.Join(root, ".claude", "worktrees", name)
	branch := "claude/" + name
	if _, err := git(root, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
		return core.Worktree{}, err
	}
	rel, err := filepath.Rel(root, originCwd)
	if err != nil {
		return core.Worktree{}, err
	}
	return core.Worktree{Cwd: filepath.Join(path, rel), Path: path, Name: name, Branch: branch, SourceBranch: src}, nil
}

// Remove never forces: git refuses when the worktree has uncommitted
// changes, so deleting a session can't silently destroy work. The branch is kept.
func (Worktrees) Remove(originCwd, path string) error {
	_, err := git(originCwd, "worktree", "remove", path)
	return err
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(text string) string {
	words := strings.Fields(nonSlug.ReplaceAllString(strings.ToLower(text), " "))
	if len(words) > 4 {
		words = words[:4]
	}
	s := strings.Join(words, "-")
	if len(s) > 30 {
		s = strings.Trim(s[:30], "-")
	}
	if s == "" {
		return "session"
	}
	return s
}
