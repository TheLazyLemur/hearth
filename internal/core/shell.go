package core

import "errors"

// OpenShell starts s's shell on a w×h screen in s's folder, or resizes the one it has: the
// client that opened or last sized the panel sets the size, as tmux's "latest" does.
func (a *App) OpenShell(s *Session, w, h int) error {
	if w < 1 || h < 1 {
		return errors.New("shell: screen too small")
	}
	l := a.L(s.SessionID)
	if l.shell != nil {
		l.shell.Resize(w, h)
		return nil
	}
	sh, err := a.shells.Open(s.SessionID, s.Cwd, w, h, a.events)
	if err != nil {
		return err
	}
	l.shell = sh
	return nil
}

// HasShell reports whether s has a shell running.
func (a *App) HasShell(s *Session) bool {
	l := a.live[s.SessionID]
	return l != nil && l.shell != nil
}

// Shell is what s's shell shows, nil when it has none.
func (a *App) Shell(s *Session) *Screen {
	if !a.HasShell(s) {
		return nil
	}
	return a.live[s.SessionID].shell.Screen()
}

// ShellKey types k into s's shell.
func (a *App) ShellKey(s *Session, k Key) error {
	if !a.HasShell(s) {
		return errNoShell
	}
	a.live[s.SessionID].shell.Key(k)
	return nil
}

// ShellPaste pastes text into s's shell.
func (a *App) ShellPaste(s *Session, text string) error {
	if !a.HasShell(s) {
		return errNoShell
	}
	a.live[s.SessionID].shell.Paste(text)
	return nil
}

// errNoShell answers input for a shell that has exited, which another client may not have seen yet.
var errNoShell = errors.New("shell exited")
