package tui

import "slices"

type editMode int

const (
	modeInsert editMode = iota
	modeNormal
)

type editorAction int

const (
	editorNoop editorAction = iota
	editorSend
	editorFocusTranscript
)

const maxUndo = 100

type undoState struct {
	text   []rune
	cursor int
}

type editor struct {
	mode             editMode
	text             []rune
	cursor           int
	scrollRow        int
	undoStack        []undoState
	pendingOp        rune
	pendingWindowCmd bool
}

func (e *editor) reset() {
	e.text = e.text[:0]
	e.undoStack = nil
	e.pendingOp, e.pendingWindowCmd = 0, false
	e.saveUndo()
	e.startInsert(0)
}

func (e *editor) handleKey(register *[]rune, char rune, key keyCode) editorAction {
	if key == keyEnter {
		return editorSend
	}
	if e.mode == modeInsert {
		return e.handleInsertKey(char, key)
	}
	return e.handleNormalKey(register, char, key)
}

func (e *editor) startInsert(at int) {
	e.mode = modeInsert
	e.cursor = clamp(at, 0, len(e.text))
}

func (e *editor) insertText(text []rune) {
	e.text = slices.Insert(e.text, e.cursor, text...)
	e.cursor += len(text)
}

func (e *editor) handleInsertKey(char rune, key keyCode) editorAction {
	switch {
	case key == keyEsc:
		e.mode = modeNormal
		if e.cursor > lineStart(e.text, e.cursor) {
			e.cursor--
		}
	case key == keyBackspace:
		if e.cursor > 0 {
			e.text = slices.Delete(e.text, e.cursor-1, e.cursor)
			e.cursor--
		}
	case key == keyCtrlU:
		start := lineStart(e.text, e.cursor)
		e.text = slices.Delete(e.text, start, e.cursor)
		e.cursor = start
	case key == keyCtrlW:
		wordStart, _, _ := motionTarget(e.text, e.cursor, 'b')
		e.text = slices.Delete(e.text, wordStart, e.cursor)
		e.cursor = wordStart
	case key == keyLeft:
		e.cursor = max(0, e.cursor-1)
	case key == keyRight:
		e.cursor = min(len(e.text), e.cursor+1)
	case key == keyUp:
		target, _, ok := motionTarget(e.text, e.cursor, 'k')
		if !ok {
			return editorFocusTranscript
		}
		e.cursor = target
	case key == keyDown:
		if target, _, ok := motionTarget(e.text, e.cursor, 'j'); ok {
			e.cursor = target
		}
	case char != 0:
		e.text = slices.Insert(e.text, e.cursor, char)
		e.cursor++
	}
	return editorNoop
}

func (e *editor) handleNormalKey(register *[]rune, char rune, key keyCode) editorAction {
	defer e.clampCursor()
	if e.pendingWindowCmd {
		e.pendingWindowCmd = false
		if char == 'k' || key == keyUp {
			return editorFocusTranscript
		}
		return editorNoop
	}
	pendingOp := e.pendingOp
	e.pendingOp = 0
	switch pendingOp {
	case 'r':
		if char != 0 && len(e.text) > 0 {
			e.saveUndo()
			e.text[e.cursor] = char
		}
		return editorNoop
	case 'd', 'c', 'y':
		e.applyOperator(register, pendingOp, char)
		return editorNoop
	}

	start, end := lineStart(e.text, e.cursor), lineEnd(e.text, e.cursor)
	switch {
	case key == keyCtrlW:
		e.pendingWindowCmd = true
	case char == 'k' || key == keyUp:
		target, _, ok := motionTarget(e.text, e.cursor, 'k')
		if !ok {
			return editorFocusTranscript
		}
		e.cursor = target
	case char == 'j' || key == keyDown:
		if target, _, ok := motionTarget(e.text, e.cursor, 'j'); ok {
			e.cursor = target
		}
	case char == 'o':
		e.saveUndo()
		e.text = slices.Insert(e.text, end, '\n')
		e.startInsert(end + 1)
	case char == 'O':
		e.saveUndo()
		e.text = slices.Insert(e.text, start, '\n')
		e.startInsert(start)
	case char == 'i':
		e.saveUndo()
		e.startInsert(e.cursor)
	case char == 'a':
		e.saveUndo()
		e.startInsert(min(e.cursor+1, end))
	case char == 'I':
		firstNonBlank, _, _ := motionTarget(e.text, e.cursor, '^')
		e.saveUndo()
		e.startInsert(firstNonBlank)
	case char == 'A':
		e.saveUndo()
		e.startInsert(end)
	case char == 'x':
		e.deleteRange(register, e.cursor, min(end, e.cursor+1))
	case char == 'X':
		e.deleteRange(register, max(start, e.cursor-1), e.cursor)
	case char == 'D':
		e.deleteRange(register, e.cursor, end)
	case char == 'C':
		e.deleteRange(register, e.cursor, end)
		e.startInsert(e.cursor)
	case char == 'p':
		after := 0
		if len(e.text) > 0 {
			after = min(e.cursor+1, end)
		}
		e.paste(register, after)
	case char == 'P':
		e.paste(register, e.cursor)
	case char == 'u':
		e.undo()
	case char == 'd' || char == 'c' || char == 'y' || char == 'r':
		e.pendingOp = char
	default:
		motion := char
		switch key {
		case keyLeft:
			motion = 'h'
		case keyRight:
			motion = 'l'
		}
		if target, _, ok := motionTarget(e.text, e.cursor, motion); ok {
			e.cursor = target
		}
	}
	return editorNoop
}

func (e *editor) applyOperator(register *[]rune, op, motion rune) {
	var from, to int
	if motion == op {
		from, to = lineStart(e.text, e.cursor), lineEnd(e.text, e.cursor)
		if op == 'd' {
			lineText := slices.Clone(e.text[from:to])
			if to < len(e.text) {
				to++
			} else if from > 0 {
				from--
			}
			e.deleteRange(register, from, to)
			*register = lineText
			e.cursor = lineStart(e.text, min(from, len(e.text)))
			return
		}
	} else {
		if op == 'c' && motion == 'w' && e.cursor < len(e.text) && !isBlank(e.text[e.cursor]) {
			motion = 'e'
		}
		target, inclusive, ok := motionTarget(e.text, e.cursor, motion)
		if !ok {
			return
		}
		from, to = min(e.cursor, target), max(e.cursor, target)
		if inclusive {
			to = min(len(e.text), to+1)
		}
	}
	switch op {
	case 'y':
		*register = slices.Clone(e.text[from:to])
		e.cursor = from
	case 'd':
		e.deleteRange(register, from, to)
	case 'c':
		e.deleteRange(register, from, to)
		e.startInsert(from)
	}
}

func (e *editor) deleteRange(register *[]rune, from, to int) {
	if from >= to {
		return
	}
	e.saveUndo()
	*register = slices.Clone(e.text[from:to])
	e.text = slices.Delete(e.text, from, to)
	e.cursor = from
}

func (e *editor) paste(register *[]rune, at int) {
	if len(*register) == 0 {
		return
	}
	e.saveUndo()
	e.text = slices.Insert(e.text, at, *register...)
	e.cursor = at + len(*register) - 1
}

func (e *editor) saveUndo() {
	if len(e.undoStack) == maxUndo {
		e.undoStack = slices.Delete(e.undoStack, 0, 1)
	}
	e.undoStack = append(e.undoStack, undoState{slices.Clone(e.text), e.cursor})
}

func (e *editor) undo() {
	if len(e.undoStack) == 0 {
		return
	}
	last := e.undoStack[len(e.undoStack)-1]
	e.undoStack = e.undoStack[:len(e.undoStack)-1]
	e.text, e.cursor = last.text, last.cursor
}

func (e *editor) clampCursor() {
	if e.mode == modeInsert {
		e.cursor = clamp(e.cursor, 0, len(e.text))
		return
	}
	e.cursor = clamp(e.cursor, 0, max(0, len(e.text)-1))
	if e.cursor < len(e.text) && e.text[e.cursor] == '\n' && e.cursor > lineStart(e.text, e.cursor) {
		e.cursor--
	}
}

func lineStart(text []rune, pos int) int {
	start := min(pos, len(text))
	for start > 0 && text[start-1] != '\n' {
		start--
	}
	return start
}

func lineEnd(text []rune, pos int) int {
	end := pos
	for end < len(text) && text[end] != '\n' {
		end++
	}
	return end
}

func motionTarget(text []rune, pos int, motion rune) (target int, inclusive, ok bool) {
	start, end := lineStart(text, pos), lineEnd(text, pos)
	column := pos - start
	switch motion {
	case 'h':
		return max(start, pos-1), false, true
	case 'l':
		return min(end, pos+1), false, true
	case '0':
		return start, false, true
	case '^':
		first := start
		for first < end && (text[first] == ' ' || text[first] == '\t') {
			first++
		}
		return first, false, true
	case '$':
		return max(start, end-1), true, true
	case 'k':
		if start == 0 {
			return pos, false, false
		}
		prevStart, prevEnd := lineStart(text, start-1), start-1
		return prevStart + min(column, prevEnd-prevStart), false, true
	case 'j':
		if end == len(text) {
			return pos, false, false
		}
		nextStart, nextEnd := end+1, lineEnd(text, end+1)
		return nextStart + min(column, nextEnd-nextStart), false, true
	case 'w':
		next := pos
		for next < len(text) && !isBlank(text[next]) {
			next++
		}
		for next < len(text) && isBlank(text[next]) {
			next++
		}
		return next, false, true
	case 'b':
		prev := pos
		for prev > 0 && isBlank(text[prev-1]) {
			prev--
		}
		for prev > 0 && !isBlank(text[prev-1]) {
			prev--
		}
		return prev, false, true
	case 'e':
		next := pos + 1
		for next < len(text) && isBlank(text[next]) {
			next++
		}
		for next+1 < len(text) && !isBlank(text[next+1]) {
			next++
		}
		return min(next, max(0, len(text)-1)), true, true
	}
	return 0, false, false
}

func isBlank(char rune) bool {
	return char == ' ' || char == '\t' || char == '\n'
}

func clamp(value, low, high int) int {
	return max(low, min(value, high))
}
