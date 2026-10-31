package core

import (
	"path/filepath"
	"slices"
	"strings"
)

// readOnlyTools are the pi-named built-in tools that change nothing, so no mode asks for them.
var readOnlyTools = []string{"read", "grep", "find", "ls"}

// Permit decides a tool call: "allow", "deny", "ask" the user, or "classify" it (auto mode).
// "read", "grep", "find" and "ls" are the read-only tools; the names are pi's, and other backends map theirs onto them.
// Calls that run without asking must keep their path inside cwd, the session's folder.
func Permit(mode, tool, path, cwd string) string {
	readOnly, in := slices.Contains(readOnlyTools, tool), within(cwd, path)
	switch {
	case readOnly && in:
		return "allow"
	case mode == "acceptEdits" && (tool == "edit" || tool == "write") && in:
		return "allow"
	case mode == "plan" && !readOnly:
		return "deny"
	case mode == "auto":
		return "classify"
	}
	return "ask"
}

// within reports whether path, relative to cwd when not absolute, stays inside cwd once symlinks resolve.
// A leading ~ counts as outside: pi may expand it to the home folder.
func within(cwd, path string) bool {
	if strings.HasPrefix(path, "~") {
		return false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	rel, err := filepath.Rel(resolve(cwd), resolve(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// resolve follows symlinks in the longest part of p that exists, so a new file under a linked folder resolves too.
func resolve(p string) string {
	p = filepath.Clean(p)
	for rest := ""; ; {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		p, rest = parent, filepath.Join(filepath.Base(p), rest)
	}
}
