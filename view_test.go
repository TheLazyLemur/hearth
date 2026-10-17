package main

import "testing"

func textLines(texts ...string) []screenLine {
	lines := make([]screenLine, len(texts))
	for i, text := range texts {
		for _, r := range text {
			lines[i].cells = append(lines[i].cells, cell{r})
		}
	}
	return lines
}

func press(v *transcriptView, lines []screenLine, height int, register *[]rune, keys string) viewAction {
	action := viewNoop
	for _, char := range keys {
		action = v.handleKey(lines, height, register, char, keyNone)
	}
	return action
}

func TestVerticalMovesClampToShortLinesAndKeepTheWantedColumn(t *testing.T) {
	// given
	// ... the cursor at column 6 of a long line above a short one
	lines := textLines("long line here", "ab", "another long line")
	v := &transcriptView{cursor: position{0, 6}, stickyCol: 6}
	var register []rune

	// when
	// ... moving down twice
	press(v, lines, 10, &register, "j")
	onShortLine := v.cursor
	press(v, lines, 10, &register, "j")

	// then
	// ... the short line clamps the column and the next long line restores it
	if onShortLine != (position{1, 1}) || v.cursor != (position{2, 6}) {
		t.Errorf("on short line %v, then %v", onShortLine, v.cursor)
	}
}

func TestWordMotionsCrossLineBoundaries(t *testing.T) {
	// given
	// ... the cursor on the last word of a line
	lines := textLines("one two", "  three four")
	v := &transcriptView{cursor: position{0, 4}}
	var register []rune

	// when
	// ... moving forward a word, then back two
	press(v, lines, 10, &register, "w")
	afterW := v.cursor
	press(v, lines, 10, &register, "bb")

	// then
	// ... w lands on the next line's first word and b walks back across it
	if afterW != (position{1, 2}) || v.cursor != (position{0, 0}) {
		t.Errorf("after w %v, after bb %v", afterW, v.cursor)
	}
}

func TestGgAndGJumpToTheEndsAndTheViewFollows(t *testing.T) {
	// given
	// ... ten lines, a three-line view, the cursor at the top
	lines := textLines("0", "1", "2", "3", "4", "5", "6", "7", "8", "9")
	v := &transcriptView{}
	var register []rune

	// when
	// ... jumping to the bottom, then back to the top
	press(v, lines, 3, &register, "G")
	bottom, bottomScroll := v.cursor.line, v.scrollTop
	press(v, lines, 3, &register, "gg")

	// then
	// ... G shows the last line at the bottom of the view and gg returns to the top
	if bottom != 9 || bottomScroll != 7 || v.cursor.line != 0 || v.scrollTop != 0 {
		t.Errorf("G %d (scroll %d), gg %d (scroll %d)", bottom, bottomScroll, v.cursor.line, v.scrollTop)
	}
}

func TestHalfPageDownMovesCursorAndViewTogether(t *testing.T) {
	// given
	// ... twenty lines and a four-line view at the top
	lines := textLines("0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "19")
	v := &transcriptView{}
	var register []rune

	// when
	// ... pressing Ctrl-d
	v.handleKey(lines, 4, &register, 0, keyCtrlD)

	// then
	// ... both move by half the view height
	if v.cursor.line != 2 || v.scrollTop != 2 {
		t.Errorf("line %d, scroll %d", v.cursor.line, v.scrollTop)
	}
}

func TestIAsksToEnterInsertMode(t *testing.T) {
	// given
	// ... view mode
	lines := textLines("hello")
	v := &transcriptView{}
	var register []rune

	// when
	// ... pressing i
	action := press(v, lines, 10, &register, "i")

	// then
	// ... the caller is told to switch to insert mode
	if action != viewInsert {
		t.Errorf("action %d", action)
	}
}

func TestYyYanksTheCursorLineIntoTheRegister(t *testing.T) {
	// given
	// ... the cursor on the second line
	lines := textLines("first", "  second line")
	v := &transcriptView{cursor: position{1, 4}}
	var register []rune

	// when
	// ... pressing yy
	action := press(v, lines, 10, &register, "yy")

	// then
	// ... the whole line is in the register
	if action != viewYanked || string(register) != "  second line" {
		t.Errorf("action %d, register %q", action, string(register))
	}
}

func TestYDollarYanksFromTheCursorToTheEndOfTheLine(t *testing.T) {
	// given
	// ... the cursor in the middle of a line
	lines := textLines("hello world")
	v := &transcriptView{cursor: position{0, 6}}
	var register []rune

	// when
	// ... pressing y$
	action := press(v, lines, 10, &register, "y$")

	// then
	// ... the rest of the line is in the register
	if action != viewYanked || string(register) != "world" {
		t.Errorf("action %d, register %q", action, string(register))
	}
}

func TestJOnTheLastLineMovesFocusToTheInput(t *testing.T) {
	// given
	// ... the cursor on the last transcript line
	lines := textLines("a", "b")
	v := &transcriptView{cursor: position{1, 0}}
	var register []rune

	// when
	// ... pressing j
	action := press(v, lines, 10, &register, "j")

	// then
	// ... the caller is told to focus the input
	if action != viewFocusInput {
		t.Errorf("action %d", action)
	}
}

func TestCtrlWJMovesFocusToTheInputFromAnywhere(t *testing.T) {
	// given
	// ... the cursor at the top of the transcript
	lines := textLines("a", "b", "c")
	v := &transcriptView{}
	var register []rune

	// when
	// ... pressing Ctrl-w then j
	v.handleKey(lines, 10, &register, 0, keyCtrlW)
	action := v.handleKey(lines, 10, &register, 'j', keyNone)

	// then
	// ... the caller is told to focus the input
	if action != viewFocusInput {
		t.Errorf("action %d", action)
	}
}

func TestSearchFindsForwardAndNRepeatsWithWraparound(t *testing.T) {
	// given
	// ... two lines containing "two" and the cursor at the start
	lines := textLines("one two", "three two")
	v := &transcriptView{}
	var register []rune

	// when
	// ... searching for "two", then repeating forward twice and backward once
	press(v, lines, 10, &register, "/two")
	v.handleKey(lines, 10, &register, 0, keyEnter)
	first := v.cursor
	press(v, lines, 10, &register, "n")
	second := v.cursor
	press(v, lines, 10, &register, "n")
	wrapped, wrapNote := v.cursor, v.note
	press(v, lines, 10, &register, "N")

	// then
	// ... matches are visited in order, n wraps to the top and N goes back
	if first != (position{0, 4}) || second != (position{1, 6}) || wrapped != (position{0, 4}) || v.cursor != (position{1, 6}) {
		t.Errorf("first %v, second %v, wrapped %v, back %v", first, second, wrapped, v.cursor)
	}
	if wrapNote != "search wrapped" {
		t.Errorf("note %q", wrapNote)
	}
}

func TestVisualLineYankJoinsSoftWrappedLinesAndDropsPrefixes(t *testing.T) {
	// given
	// ... a bullet wrapped onto a second screen line, then another line
	lines := textLines("• one two", "  three", "next")
	lines[0].prefixLen, lines[1].prefixLen = 2, 2
	lines[1].breakBefore = breakAtSpace
	v := &transcriptView{}
	var register []rune

	// when
	// ... selecting three lines linewise and yanking
	press(v, lines, 10, &register, "Vjjy")

	// then
	// ... the wrap becomes a space, the hard break a newline, and the bullet is left out
	if string(register) != "one two three\nnext" || v.visual != visualOff {
		t.Errorf("register %q, visual %d", string(register), v.visual)
	}
}

func TestVisualCharYankCoversCursorToMotionInclusive(t *testing.T) {
	// given
	// ... the cursor at the start of "hello world"
	lines := textLines("hello world")
	v := &transcriptView{}
	var register []rune

	// when
	// ... selecting six characters and yanking
	press(v, lines, 10, &register, "vllllly")

	// then
	// ... both ends are included
	if string(register) != "hello " {
		t.Errorf("register %q", string(register))
	}
}

func TestBracesJumpBetweenEntryStarts(t *testing.T) {
	// given
	// ... three entries starting at lines 0, 2 and 5, cursor on line 3
	lines := textLines("a", "", "b", "b", "", "c", "c")
	lines[0].startsEntry, lines[2].startsEntry, lines[5].startsEntry = true, true, true
	v := &transcriptView{cursor: position{3, 0}}
	var register []rune

	// when
	// ... jumping forward, then back twice
	press(v, lines, 10, &register, "}")
	forward := v.cursor.line
	press(v, lines, 10, &register, "{{")

	// then
	// ... } lands on the next entry and { walks back to earlier ones
	if forward != 5 || v.cursor.line != 0 {
		t.Errorf("} %d, {{ %d", forward, v.cursor.line)
	}
}

func TestEMovesToTheEndOfTheNextWordAcrossLines(t *testing.T) {
	// given
	// ... the cursor at the end of the last word on a line
	lines := textLines("one two", "  three")
	v := &transcriptView{cursor: position{0, 6}}
	var register []rune

	// when
	// ... pressing e
	press(v, lines, 10, &register, "e")

	// then
	// ... it lands on the last letter of the next line's first word
	if v.cursor != (position{1, 6}) {
		t.Errorf("at %v", v.cursor)
	}
}
