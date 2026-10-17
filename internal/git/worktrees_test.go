package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	return dir
}

func TestCreateCutsClaudeBranchUnderDotClaudeWorktrees(t *testing.T) {
	// given
	// ... a repo on main
	repo := gitRepo(t)

	// when
	// ... a worktree is created for a prompt
	wt, err := Worktrees{}.Create(repo, "Update README.md")

	// then
	// ... it sits under .claude/worktrees on a claude/ branch named after the prompt, cut from main
	if err != nil || !strings.HasPrefix(wt.Path, filepath.Join(repo, ".claude", "worktrees", "update-readme-md-")) ||
		!strings.HasPrefix(wt.Branch, "claude/update-readme-md-") || wt.SourceBranch != "main" || wt.Cwd != wt.Path {
		t.Fatalf("err %v worktree %+v", err, wt)
	}
}

func TestRemoveRefusesDirtyWorktree(t *testing.T) {
	// given
	// ... a worktree with an uncommitted file
	repo := gitRepo(t)
	wt, err := Worktrees{}.Create(repo, "dirty work")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wt.Path, "wip.txt"), []byte("unsaved"), 0o600)

	// when
	// ... it is removed
	err = Worktrees{}.Remove(repo, wt.Path)

	// then
	// ... removal fails and the uncommitted file remains
	_, statErr := os.Stat(filepath.Join(wt.Path, "wip.txt"))
	if err == nil || statErr != nil {
		t.Fatalf("err %v stat %v", err, statErr)
	}
}
