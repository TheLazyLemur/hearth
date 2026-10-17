package store

import (
	"testing"

	"hearth/internal/core"
)

func TestStoreRoundTrip(t *testing.T) {
	// given
	// ... a store holding one starred, archived session
	st := Store{t.TempDir()}
	s := &core.Session{SessionID: "local_1", Cwd: "/x", Title: "t", IsStarred: true, IsArchived: true, PermissionMode: "plan"}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}

	// when
	// ... loaded back and then deleted
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	delErr := st.Delete("local_1")
	after, _ := st.Load()

	// then
	// ... fields survive the round trip and deletion empties the store
	if len(got) != 1 || *got[0] != *s || delErr != nil || len(after) != 0 {
		t.Fatalf("got %+v, delete err %v, after %d", got, delErr, len(after))
	}
}
