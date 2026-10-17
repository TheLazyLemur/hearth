package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"hearth/internal/core"
)

var _ core.SessionStore = Store{}

// Store keeps one JSON file per session in Dir.
type Store struct{ Dir string }

func (s Store) path(id string) string { return filepath.Join(s.Dir, id+".json") }

func (s Store) Load() ([]*core.Session, error) {
	files, err := filepath.Glob(filepath.Join(s.Dir, "local_*.json"))
	if err != nil {
		return nil, err
	}
	var out []*core.Session
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var ss core.Session
		if err := json.Unmarshal(b, &ss); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, &ss)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActivityAt > out[j].LastActivityAt })
	return out, nil
}

// Save writes atomically so a crash mid-write never leaves a truncated session file.
func (s Store) Save(ss *core.Session) error {
	if !strings.HasPrefix(ss.SessionID, "local_") {
		panic("session id must start with local_: " + ss.SessionID)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ss, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(ss.SessionID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(ss.SessionID))
}

func (s Store) Delete(id string) error { return os.Remove(s.path(id)) }
