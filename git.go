package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// createWorktree mirrors the desktop layout: <repo>/.claude/worktrees/<slug>-<hex>
// on branch claude/<slug>-<hex>, cut from HEAD. Returns the cwd to run in
// (the origin's sub-path inside the new worktree).
func createWorktree(s *Session, prompt string) error {
	root, err := git(s.OriginCwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	src, _ := git(root, "rev-parse", "--abbrev-ref", "HEAD")
	name := slugify(prompt) + "-" + randomHex(6)
	path := filepath.Join(root, ".claude", "worktrees", name)
	branch := "claude/" + name
	if _, err := git(root, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
		return err
	}
	rel, err := filepath.Rel(root, s.OriginCwd)
	if err != nil {
		return err
	}
	s.Cwd, s.WorktreePath, s.WorktreeName, s.Branch, s.SourceBranch = filepath.Join(path, rel), path, name, branch, src
	return nil
}

// removeWorktree never forces: git refuses when the worktree has uncommitted
// changes, so deleting a session can't silently destroy work. The branch is kept.
func removeWorktree(s *Session) error {
	_, err := git(s.OriginCwd, "worktree", "remove", s.WorktreePath)
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
