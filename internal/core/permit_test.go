package core_test

import (
	"os"
	"path/filepath"
	"testing"

	"hearth/internal/core"
)

func TestPermissionModeAndPathDecideWhichToolsAsk(t *testing.T) {
	cases := []struct{ mode, tool, path, want string }{
		{"default", "read", "a.go", "allow"},
		{"default", "ls", "", "allow"},
		{"default", "read", "/proj/sub/a.go", "allow"},
		{"default", "read", "/etc/passwd", "ask"},
		{"default", "read", "../other/a.go", "ask"},
		{"default", "read", "~/.aws/credentials", "ask"},
		{"default", "bash", "", "ask"},
		{"default", "edit", "a.go", "ask"},
		{"default", "mcp__jira__create", "", "ask"},
		{"acceptEdits", "edit", "a.go", "allow"},
		{"acceptEdits", "write", "sub/new.go", "allow"},
		{"acceptEdits", "write", "/Users/x/.zshrc", "ask"},
		{"acceptEdits", "bash", "", "ask"},
		{"plan", "grep", "", "allow"},
		{"plan", "read", "/etc/passwd", "ask"},
		{"plan", "bash", "", "deny"},
		{"plan", "write", "a.go", "deny"},
		{"auto", "read", "a.go", "allow"},
		{"auto", "bash", "", "classify"},
		{"auto", "read", "/etc/passwd", "classify"},
	}
	for _, c := range cases {
		t.Run(c.mode+" "+c.tool+" "+c.path, func(t *testing.T) {
			// given
			// ... a session in /proj in a permission mode
			mode, cwd := c.mode, "/proj"

			// when
			// ... a tool is about to run on a path
			got := core.Permit(mode, c.tool, c.path, cwd)

			// then
			// ... the mode and whether the path stays in the project decide what happens
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestSymlinkOutOfTheProjectCountsAsOutside(t *testing.T) {
	// given
	// ... a project with a link to a folder outside it
	cwd, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cwd, "link")); err != nil {
		t.Fatal(err)
	}

	// when
	// ... a tool reads and writes through the link
	read := core.Permit("default", "read", "link/secret", cwd)
	write := core.Permit("acceptEdits", "write", "link/new/file.go", cwd)

	// then
	// ... both ask, as for any path outside the project
	if read != "ask" || write != "ask" {
		t.Fatalf("read %q write %q", read, write)
	}
}
