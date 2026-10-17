package tui

import "testing"

func editorWith(text string, cursor int) *editor {
	return &editor{mode: modeNormal, text: []rune(text), cursor: cursor}
}

func feed(e *editor, reg *[]rune, keys string) editorAction {
	action := editorNoop
	for _, r := range keys {
		action = e.handleKey(reg, r, keyNone)
	}
	return action
}

func TestDwDeletesAWordIntoTheRegisterAndPPutsItBack(t *testing.T) {
	// given
	// ... the cursor on the first word
	e := editorWith("one two three", 0)
	var register []rune

	// when
	// ... deleting a word, then pasting before the cursor
	feed(e, &register, "dw")
	afterDw := string(e.text)
	feed(e, &register, "P")

	// then
	// ... the word and its space were removed, then restored
	if afterDw != "two three" || string(register) != "one " || string(e.text) != "one two three" {
		t.Errorf("after dw %q, register %q, after P %q", afterDw, string(register), string(e.text))
	}
}

func TestCwChangesToTheEndOfTheWordAndEscReturnsToNormal(t *testing.T) {
	// given
	// ... the cursor on the first word
	e := editorWith("one two", 0)
	var register []rune

	// when
	// ... changing the word and leaving insert mode
	feed(e, &register, "cwONE")
	e.handleKey(&register, 0, keyEsc)

	// then
	// ... only the word is replaced, the space is kept, the cursor is on its last letter
	if string(e.text) != "ONE two" || e.mode != modeNormal || e.cursor != 2 {
		t.Errorf("text %q, mode %d, cursor %d", string(e.text), e.mode, e.cursor)
	}
}

func TestUUndoesAWholeInsertSession(t *testing.T) {
	// given
	// ... text appended in one insert session
	e := editorWith("abc", 2)
	var register []rune
	feed(e, &register, "Adef")
	e.handleKey(&register, 0, keyEsc)

	// when
	// ... undoing once
	feed(e, &register, "u")

	// then
	// ... the whole insert is gone
	if string(e.text) != "abc" {
		t.Errorf("text %q", string(e.text))
	}
}

func TestKInNormalModeMovesFocusToTheTranscript(t *testing.T) {
	// given
	// ... normal mode on the input
	e := editorWith("hello", 0)
	var register []rune

	// when
	// ... pressing k
	action := feed(e, &register, "k")

	// then
	// ... the caller is told to focus the transcript
	if action != editorFocusTranscript {
		t.Errorf("action %d", action)
	}
}

func TestEnterSendsFromInsertMode(t *testing.T) {
	// given
	// ... text typed in insert mode
	e := editorWith("", 0)
	e.mode = modeInsert
	var register []rune
	feed(e, &register, "hi")

	// when
	// ... pressing Enter
	action := e.handleKey(&register, 0, keyEnter)

	// then
	// ... the caller is told to send
	if action != editorSend {
		t.Errorf("action %d", action)
	}
}

func TestKMovesUpALineKeepingTheColumnAndLeavesFromTheTop(t *testing.T) {
	// given
	// ... two lines with the cursor on the second character of the second
	e := editorWith("ab\ncd", 4)
	var register []rune

	// when
	// ... pressing k twice
	first := feed(e, &register, "k")
	afterFirst := e.cursor
	second := feed(e, &register, "k")

	// then
	// ... the first k moves up in the input, the second leaves it
	if first != editorNoop || afterFirst != 1 || second != editorFocusTranscript {
		t.Errorf("first %d at %d, second %d", first, afterFirst, second)
	}
}

func TestDdDeletesOnlyTheCursorLine(t *testing.T) {
	// given
	// ... three lines with the cursor on the middle one
	e := editorWith("one\ntwo\nthree", 5)
	var register []rune

	// when
	// ... pressing dd
	feed(e, &register, "dd")

	// then
	// ... the middle line and its newline are gone and the cursor is on the next line
	if string(e.text) != "one\nthree" || e.cursor != 4 {
		t.Errorf("text %q, cursor %d", string(e.text), e.cursor)
	}
}

func TestZeroAndDollarStayOnTheCursorLine(t *testing.T) {
	// given
	// ... the cursor at the start of the second line
	e := editorWith("ab\ncdef", 3)
	var register []rune

	// when
	// ... jumping to the end of the line and back
	feed(e, &register, "$")
	atEnd := e.cursor
	feed(e, &register, "0")

	// then
	// ... both stop at the ends of that line, not the whole input
	if atEnd != 6 || e.cursor != 3 {
		t.Errorf("$ at %d, 0 at %d", atEnd, e.cursor)
	}
}

func TestOOpensANewLineBelowInInsertMode(t *testing.T) {
	// given
	// ... two lines with the cursor on the first
	e := editorWith("ab\ncd", 0)
	var register []rune

	// when
	// ... pressing o and typing
	feed(e, &register, "oxy")

	// then
	// ... the text goes on a new line between the two
	if string(e.text) != "ab\nxy\ncd" || e.mode != modeInsert {
		t.Errorf("text %q, mode %d", string(e.text), e.mode)
	}
}
