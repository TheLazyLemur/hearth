package main

import (
	"fmt"
	"math"
	"slices"
	"unicode"
)

type position struct {
	line, col int
}

func (p position) before(other position) bool {
	return p.line < other.line || (p.line == other.line && p.col < other.col)
}

const endOfLine = math.MaxInt

type visualMode int

const (
	visualOff visualMode = iota
	visualChar
	visualLine
)

type viewAction int

const (
	viewNoop viewAction = iota
	viewInsert
	viewFocusInput
	viewYanked
	viewQuit
	viewToggleFold
	viewExpandAll
	viewCollapseAll
	viewCommand
	viewGoToFile
)

type search struct {
	typing        bool
	typed         []rune
	typedBackward bool
	pattern       []rune
	backward      bool
}

type transcriptView struct {
	cursor           position
	stickyCol        int
	scrollTop        int
	pendingKey       rune
	pendingWindowCmd bool

	visual visualMode
	anchor position
	search search

	note string
}

func (v *transcriptView) handleKey(lines []screenLine, height int, register *[]rune, char rune, key keyCode) viewAction {
	if v.search.typing {
		v.handleSearchKey(lines, height, char, key)
		return viewNoop
	}

	pendingKey := v.pendingKey
	v.pendingKey = 0
	if v.pendingWindowCmd {
		v.pendingWindowCmd = false
		if char == 'j' || key == keyDown {
			v.visual = visualOff
			return viewFocusInput
		}
		return viewNoop
	}

	if pendingKey == 'z' {
		switch char {
		case 'a':
			return viewToggleFold
		case 'R':
			return viewExpandAll
		case 'M':
			return viewCollapseAll
		}
		return viewNoop
	}

	switch {
	case key == keyCtrlW:
		v.pendingWindowCmd = true
		return viewNoop
	case v.visual == visualOff && pendingKey == 0 && char == 'z':
		v.pendingKey = 'z'
		return viewNoop
	case key == keyEsc:
		if v.visual != visualOff {
			v.visual = visualOff
		} else {
			v.search.pattern = nil
		}
		return viewNoop
	case v.visual == visualOff && (char == 'i' || char == 'a' || char == 'o'):
		return viewInsert
	case v.visual == visualOff && char == 'q':
		return viewQuit
	case char == '/' || char == '?':
		v.search.typing = true
		v.search.typedBackward = char == '?'
		v.search.typed = nil
		return viewNoop
	case char == ':':
		return viewCommand
	}
	if len(lines) == 0 {
		return viewNoop
	}

	if v.visual != visualOff {
		if action, handled := v.handleVisualKey(lines, height, register, char); handled {
			return action
		}
	} else {
		if pendingKey == 'y' {
			return v.yankFromCursor(lines, register, char)
		}
		switch char {
		case 'y':
			v.pendingKey = 'y'
			return viewNoop
		case 'v':
			v.visual, v.anchor = visualChar, v.cursor
			return viewNoop
		case 'V':
			v.visual, v.anchor = visualLine, v.cursor
			return viewNoop
		}
		if (char == 'j' || key == keyDown) && v.cursor.line == len(lines)-1 {
			return viewFocusInput
		}
	}

	if pendingKey == 'g' && char == 'f' {
		return viewGoToFile
	}

	switch char {
	case 'n', 'N':
		v.jumpToMatch(lines, height, v.search.backward != (char == 'N'))
		return viewNoop
	case '}':
		next := v.cursor.line + 1
		for next < len(lines)-1 && !lines[next].startsEntry {
			next++
		}
		v.jumpTo(lines, height, position{min(next, len(lines)-1), 0})
		return viewNoop
	case '{':
		prev := v.cursor.line - 1
		for prev > 0 && !lines[prev].startsEntry {
			prev--
		}
		v.jumpTo(lines, height, position{max(prev, 0), 0})
		return viewNoop
	}

	v.move(lines, height, pendingKey, char, key)
	return viewNoop
}

func (v *transcriptView) handleVisualKey(lines []screenLine, height int, register *[]rune, char rune) (action viewAction, handled bool) {
	switch char {
	case 'y':
		start, end := v.selection()
		*register = yankText(lines, start, end, v.visual == visualLine)
		v.visual = visualOff
		v.jumpTo(lines, height, start)
		return viewYanked, true
	case 'o':
		v.cursor, v.anchor = v.anchor, v.cursor
		v.stickyCol = v.cursor.col
		v.clampAndScroll(lines, height)
		return viewNoop, true
	case 'v', 'V':
		want := visualChar
		if char == 'V' {
			want = visualLine
		}
		if v.visual == want {
			v.visual = visualOff
		} else {
			v.visual = want
		}
		return viewNoop, true
	}
	return viewNoop, false
}

func (v *transcriptView) yankFromCursor(lines []screenLine, register *[]rune, char rune) viewAction {
	line := v.cursor.line
	switch char {
	case 'y':
		*register = yankText(lines, position{line, 0}, position{line, endOfLine}, true)
	case '$':
		*register = yankText(lines, v.cursor, position{line, endOfLine}, false)
	default:
		return viewNoop
	}
	return viewYanked
}

func (v *transcriptView) move(lines []screenLine, height int, pendingKey, char rune, key keyCode) {
	halfPage := max(1, height/2)
	vertical := false
	scroll := func(by int) {
		v.cursor.line += by
		v.scrollTop += by
		vertical = true
	}
	switch {
	case char == 'j' || key == keyDown:
		v.cursor.line++
		vertical = true
	case char == 'k' || key == keyUp:
		v.cursor.line--
		vertical = true
	case char == 'h' || key == keyLeft:
		v.cursor.col--
	case char == 'l' || key == keyRight:
		v.cursor.col++
	case char == '0':
		v.cursor.col = 0
	case char == '$':
		v.cursor.col = endOfLine
		v.stickyCol = endOfLine
		vertical = true
	case char == 'w':
		v.cursor = nextWordStart(lines, v.cursor)
	case char == 'b':
		v.cursor = prevWordStart(lines, v.cursor)
	case char == 'e':
		v.cursor = nextWordEnd(lines, v.cursor)
	case char == 'G':
		v.cursor.line = len(lines) - 1
		vertical = true
	case char == 'g':
		if pendingKey != 'g' {
			v.pendingKey = 'g'
			return
		}
		v.cursor.line = 0
		vertical = true
	case key == keyCtrlD:
		scroll(halfPage)
	case key == keyCtrlU:
		scroll(-halfPage)
	case key == keyCtrlF || key == keyPageDown:
		scroll(height)
	case key == keyCtrlB || key == keyPageUp:
		scroll(-height)
	default:
		return
	}

	if vertical {
		v.cursor.col = v.stickyCol
	}
	v.clampAndScroll(lines, height)
	if !vertical {
		v.stickyCol = v.cursor.col
	}
}

func (v *transcriptView) jumpTo(lines []screenLine, height int, target position) {
	v.cursor = target
	v.clampAndScroll(lines, height)
	v.stickyCol = v.cursor.col
}

func (v *transcriptView) focusLastLine(lines []screenLine, height int) {
	v.pendingKey = 0
	v.pendingWindowCmd = false
	last := len(lines) - 1
	for last > 0 && len(lines[last].cells) == 0 {
		last--
	}
	v.cursor, v.stickyCol = position{last, 0}, 0
	v.clampAndScroll(lines, height)
}

func (v *transcriptView) clampAndScroll(lines []screenLine, height int) {
	if len(lines) == 0 {
		v.cursor, v.scrollTop = position{}, 0
		return
	}
	v.cursor.line = clamp(v.cursor.line, 0, len(lines)-1)
	v.cursor.col = clamp(v.cursor.col, 0, max(0, len(lines[v.cursor.line].cells)-1))
	v.scrollTop = clamp(v.scrollTop, 0, max(0, len(lines)-height))
	if v.cursor.line < v.scrollTop {
		v.scrollTop = v.cursor.line
	}
	if v.cursor.line >= v.scrollTop+height {
		v.scrollTop = v.cursor.line - height + 1
	}
}

func nextWordStart(lines []screenLine, from position) position {
	line, col := from.line, from.col
	cells := lines[line].cells
	for col < len(cells) && !isBlank(cells[col].char) {
		col++
	}
	for {
		for col < len(cells) && isBlank(cells[col].char) {
			col++
		}
		if col < len(cells) {
			break
		}
		if line == len(lines)-1 {
			col = max(0, len(cells)-1)
			break
		}
		line++
		cells = lines[line].cells
		col = 0
		if len(cells) == 0 {
			break
		}
	}
	return position{line, col}
}

func nextWordEnd(lines []screenLine, from position) position {
	line, col := from.line, from.col+1
	for {
		cells := lines[line].cells
		for col < len(cells) && isBlank(cells[col].char) {
			col++
		}
		if col < len(cells) {
			break
		}
		if line == len(lines)-1 {
			return position{from.line, max(0, len(cells)-1)}
		}
		line, col = line+1, 0
	}
	cells := lines[line].cells
	for col+1 < len(cells) && !isBlank(cells[col+1].char) {
		col++
	}
	return position{line, col}
}

func prevWordStart(lines []screenLine, from position) position {
	line, col := from.line, from.col
	for {
		col--
		if col < 0 {
			if line == 0 {
				return position{0, 0}
			}
			line--
			col = len(lines[line].cells) - 1
			if col < 0 {
				return position{line, 0}
			}
		}
		if !isBlank(lines[line].cells[col].char) {
			break
		}
	}
	for col > 0 && !isBlank(lines[line].cells[col-1].char) {
		col--
	}
	return position{line, col}
}

func (v *transcriptView) handleSearchKey(lines []screenLine, height int, char rune, key keyCode) {
	s := &v.search
	switch {
	case key == keyEsc:
		s.typing = false
	case key == keyEnter:
		s.typing = false
		if len(s.typed) > 0 {
			s.pattern = slices.Clone(s.typed)
			s.backward = s.typedBackward
		}
		v.jumpToMatch(lines, height, s.backward)
	case key == keyBackspace:
		if len(s.typed) == 0 {
			s.typing = false
		} else {
			s.typed = s.typed[:len(s.typed)-1]
		}
	case char != 0:
		s.typed = append(s.typed, char)
	}
}

func (v *transcriptView) jumpToMatch(lines []screenLine, height int, backward bool) {
	pattern := v.search.pattern
	if len(pattern) == 0 {
		return
	}
	var matches []position
	for i, line := range lines {
		for _, col := range matchStarts(line.cells, pattern, ignoresCase(pattern)) {
			matches = append(matches, position{i, col})
		}
	}
	if len(matches) == 0 {
		v.note = fmt.Sprintf("pattern not found: %s", string(pattern))
		return
	}

	var target position
	found := false
	if backward {
		for _, match := range slices.Backward(matches) {
			if match.before(v.cursor) {
				target, found = match, true
				break
			}
		}
	} else {
		for _, match := range matches {
			if v.cursor.before(match) {
				target, found = match, true
				break
			}
		}
	}
	if !found {
		v.note = "search wrapped"
		target = matches[0]
		if backward {
			target = matches[len(matches)-1]
		}
	}
	v.jumpTo(lines, height, target)
}

func matchStarts(cells []cell, pattern []rune, ignoreCase bool) []int {
	var starts []int
	if len(pattern) == 0 {
		return starts
	}
next:
	for start := 0; start <= len(cells)-len(pattern); start++ {
		for i, want := range pattern {
			got := cells[start+i].char
			if ignoreCase {
				got = unicode.ToLower(got)
			}
			if got != want {
				continue next
			}
		}
		starts = append(starts, start)
	}
	return starts
}

func ignoresCase(pattern []rune) bool {
	return !slices.ContainsFunc(pattern, unicode.IsUpper)
}

func (v *transcriptView) selection() (start, end position) {
	start, end = v.anchor, v.cursor
	if end.before(start) {
		start, end = end, start
	}
	if v.visual == visualLine {
		start.col, end.col = 0, endOfLine
	}
	return start, end
}

func yankText(lines []screenLine, start, end position, linewise bool) []rune {
	var text []rune
	wrote := false
	var block *string // the markdown chunk the previous row came from
	for i := start.line; i <= end.line; i++ {
		line := lines[i]
		// Skip decorations whole, including the line break they would add.
		if line.decoration || line.source != nil && line.source == block {
			continue
		}
		// Rendered markdown yanks as its raw chunk, whole: chunks carry their own
		// trailing blank lines, so one follows another with no added break.
		if line.source != nil {
			if wrote && block == nil && line.breakBefore == breakHard {
				text = append(text, '\n')
			}
			text, wrote, block = append(text, []rune(*line.source)...), true, line.source
			continue
		}
		block = nil
		if wrote {
			switch line.breakBefore {
			case breakHard:
				text = append(text, '\n')
			case breakAtSpace:
				text = append(text, ' ')
			}
		}
		wrote = true
		from, to := line.prefixLen, len(line.cells)
		if !linewise {
			if i == start.line {
				from = max(from, start.col)
			}
			if i == end.line && end.col != endOfLine {
				to = min(to, end.col+1)
			}
		}
		for _, c := range line.cells[min(from, len(line.cells)):max(from, to)] {
			text = append(text, c.char)
		}
	}
	if block != nil { // ended inside a reply: drop the gap before its next chunk
		for len(text) > 0 && text[len(text)-1] == '\n' {
			text = text[:len(text)-1]
		}
	}
	return text
}

// Ported from grimalkin's renderer: the transcript as the view's cursor sees it.
type cell struct{ char rune }

type lineBreak int

const (
	breakHard lineBreak = iota
	breakAtSpace
	breakInWord
)

type screenLine struct {
	cells       []cell
	prefixLen   int // marker or indent runes that yanks leave out
	breakBefore lineBreak
	startsEntry bool
	decoration  bool    // not content (the logo, the spinner): yanks skip it
	source      *string // raw markdown the row was rendered from, nil for plain rows
	block       *string // markdown chunk the row shows, rendered or raw, nil for plain rows
}
