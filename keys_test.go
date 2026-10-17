package main

import (
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestNewlineChordsAreTranslatedApartFromEnter(t *testing.T) {
	// given
	// ... Enter and the three newline chords as Ultraviolet reports them
	presses := []uv.KeyPressEvent{
		{Code: uv.KeyEnter},
		{Code: uv.KeyEnter, Mod: uv.ModShift},
		{Code: uv.KeyEnter, Mod: uv.ModAlt},
		{Code: 'j', Mod: uv.ModCtrl},
	}

	// when
	// ... each is translated
	var got []keyCode
	for _, p := range presses {
		_, code := translateKey(p)
		got = append(got, code)
	}

	// then
	// ... Enter sends and the chords type a newline
	want := []keyCode{keyEnter, keyNewline, keyNewline, keyNewline}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("press %d: got %d, want %d", i, got[i], want[i])
		}
	}
}
