package tui

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"hearth/internal/core"
)

// picker is a snacks-style floating finder: sessions by fuzzy title, or a grep
// through every session's messages. The query starts in INSERT; esc drops to
// NORMAL for j/k, and a second esc closes it.
type picker struct {
	grep   bool
	query  []rune
	normal bool
	pendG  bool // first g of gg
	sel    int
	items  []pickItem
}

type pickItem struct {
	s     *core.Session
	text  []rune // the row after its glyph (sessions) or title (grep)
	hits  []int  // matched indexes into text
	dim   int    // sessions: text from here on (project, branch) is dimmed
	entry int    // grep: index into the session's entries
	who   string // grep: ❯ or ⏺
	score int
}

const grepCap = 500

// leader runs the space chords that open the pickers: space space, space /.
// lead is whether the previous key was a lone space.
func (u *UI) leader(k uv.KeyPressEvent, lead bool) bool {
	switch {
	case lead && k.MatchString("space"):
		u.openPicker(false, "")
	case lead && k.String() == "/":
		u.openPicker(true, "")
	case !lead && k.MatchString("space"):
		u.lead = true
	default:
		return false
	}
	return true
}

// idleNormal reports whether space is free for the picker chords: the sidebar,
// or the session pane in NORMAL or VIEW with nothing pending.
func (u *UI) idleNormal(s *core.Session) bool {
	if s == nil || u.pane != paneSession {
		return u.op == ""
	}
	if l := u.app.L(s.SessionID); len(l.Perms) > 0 && l.Perms[0].Answers == nil {
		return false
	}
	if u.focus == focusView {
		tv := u.tv
		return !tv.search.typing && tv.visual == visualOff && tv.pendingKey == 0 && !tv.pendingWindowCmd && u.op == ""
	}
	return u.ed.mode == modeNormal && u.ed.pendingOp == 0 && !u.ed.pendingWindowCmd
}

func (u *UI) openPicker(grep bool, query string) {
	u.pick = &picker{grep: grep, query: []rune(query)}
	// shortcut: loads every transcript on open; if big stores stall it, prefilter
	// files with rg and parse only the hits, off the UI goroutine.
	if grep {
		for _, s := range u.app.Visible(false) {
			u.app.EnsureLoaded(s)
		}
	}
	u.refreshPicker()
}

func (u *UI) refreshPicker() {
	p := u.pick
	if p.grep {
		p.items = grepItems(u.app, p.query)
	} else {
		p.items = sessionItems(u.app.Visible(false), p.query)
	}
	p.sel = 0
}

// sessionItems fuzzy-matches query against each session's title, project and
// branch, best first; ties keep vs's order (starred, then most recent).
func sessionItems(vs []*core.Session, query []rune) []pickItem {
	var out []pickItem
	for _, s := range vs {
		title := cmp.Or(s.Title, "New session")
		text := []rune(title + "  " + filepath.Base(s.OriginCwd))
		if s.Branch != "" {
			text = append(text, []rune("  ⎇ "+strings.TrimPrefix(s.Branch, "claude/"))...)
		}
		if score, hits, ok := fuzzy(query, text); ok {
			out = append(out, pickItem{s: s, text: text, hits: hits, dim: len([]rune(title)), score: score})
		}
	}
	slices.SortStableFunc(out, func(a, b pickItem) int { return b.score - a.score })
	return out
}

// fuzzy matches query as a subsequence of text, ignoring case unless query has
// a capital. Consecutive letters and word starts score higher.
// shortcut: takes the first match greedily rather than the best one, fine for titles.
func fuzzy(query, text []rune) (score int, hits []int, ok bool) {
	fold := ignoresCase(query)
	q := 0
	for i, r := range text {
		if q == len(query) {
			break
		}
		if fold {
			r = unicode.ToLower(r)
		}
		if r != query[q] {
			continue
		}
		score++
		if n := len(hits); n > 0 && hits[n-1] == i-1 {
			score += 5
		}
		if i == 0 || !unicode.IsLetter(text[i-1]) && !unicode.IsDigit(text[i-1]) {
			score += 3
		}
		hits = append(hits, i)
		q++
	}
	return score, hits, q == len(query)
}

// grepItems finds query, literally and with the transcript search's case rule,
// in every line of the visible sessions' messages: one item per matching line.
func grepItems(app *core.App, query []rune) []pickItem {
	if len(query) == 0 {
		return nil
	}
	fold := ignoresCase(query)
	var out []pickItem
	for _, s := range app.Visible(false) {
		for i, e := range app.L(s.SessionID).Entries {
			who := "❯ "
			switch e.Kind {
			case "user":
			case "assistant":
				who = "⏺ "
			default:
				continue
			}
			for _, l := range strings.Split(e.Text, "\n") {
				text := []rune(strings.TrimSpace(l))
				starts := matchStarts(cellsOf(text), query, fold)
				if len(starts) == 0 {
					continue
				}
				cut, off := max(0, starts[0]-20), 0 // keep the first match in view
				if cut > 0 {
					text, off = append([]rune("…"), text[cut:]...), 1
				}
				var hits []int
				for _, m := range starts {
					for c := max(m, cut); c < m+len(query); c++ {
						hits = append(hits, c-cut+off)
					}
				}
				out = append(out, pickItem{s: s, text: text, hits: hits, entry: i, who: who})
				if len(out) == grepCap {
					return out
				}
			}
		}
	}
	return out
}

func cellsOf(text []rune) []cell {
	out := make([]cell, len(text))
	for i, r := range text {
		out[i] = cell{r}
	}
	return out
}

func (u *UI) pickKey(k uv.KeyPressEvent) {
	p := u.pick
	char, key := translateKey(k)
	move := func(d int) { p.sel = clamp(p.sel+d, 0, max(0, len(p.items)-1)) }
	switch {
	case key == keyEnter:
		u.pickOpen()
		return
	case k.MatchString("ctrl+c"):
		u.pick = nil
		return
	case k.MatchString("down"), k.MatchString("ctrl+n"), k.MatchString("ctrl+j"):
		move(1)
		return
	case k.MatchString("up"), k.MatchString("ctrl+p"), k.MatchString("ctrl+k"):
		move(-1)
		return
	}
	if p.normal {
		g := p.pendG
		p.pendG = false
		switch {
		case key == keyEsc, char == 'q':
			u.pick = nil
		case char == 'j':
			move(1)
		case char == 'k':
			move(-1)
		case key == keyCtrlD:
			move(10)
		case key == keyCtrlU:
			move(-10)
		case char == 'g' && g:
			p.sel = 0
		case char == 'g':
			p.pendG = true
		case char == 'G':
			move(len(p.items))
		case char == 'i', char == 'a', char == '/':
			p.normal = false
		}
		return
	}
	switch {
	case key == keyEsc:
		p.normal = true
		return
	case key == keyBackspace && len(p.query) > 0:
		p.query = p.query[:len(p.query)-1]
	case key == keyCtrlW:
		q := strings.TrimRight(string(p.query), " ")
		p.query = []rune(q[:strings.LastIndex(q, " ")+1])
	case key == keyCtrlU:
		p.query = nil
	case char != 0:
		p.query = append(p.query, char)
	default:
		return
	}
	u.refreshPicker()
}

// pickOpen opens the highlighted session; a grep hit lands in VIEW on the match,
// with the query as the search pattern so n and N carry on from there.
func (u *UI) pickOpen() {
	p := u.pick
	if len(p.items) == 0 {
		return
	}
	it := p.items[p.sel]
	u.pick = nil
	u.open(it.s)
	u.pane = paneSession
	if p.grep {
		u.focus = focusView
		u.tv.search.pattern = p.query
		u.jump = it.entry + 1
	}
}

// matchPos is where the transcript shows entry (index+1): its first row
// holding pat, else its first row.
func matchPos(lines []screenLine, entry int, pat []rune) position {
	first := -1
	for i, l := range lines {
		if l.entry != entry {
			continue
		}
		if first < 0 {
			first = i
		}
		if m := matchStarts(l.cells, pat, ignoresCase(pat)); len(m) > 0 {
			return position{line: i, col: m[0]}
		}
	}
	return position{line: max(first, 0)}
}

func (u *UI) drawPicker(W, H int) {
	p := u.pick
	w, h := min(W-4, 140), min(H-2, 32)
	x0, y0 := (W-w)/2, (H-h)/2
	lw := w * 2 / 5
	u.ambient = th.card
	defer func() { u.ambient = nil }()
	title := "sessions"
	if p.grep {
		title = "grep"
	}
	u.box(x0, y0, lw, h, title)
	u.box(x0+lw, y0, w-lw, h, "")
	u.drawLine(x0+2, y0+1, lw-4, ln(span{"> ", fg(th.accent)}, span{string(p.query), uv.Style{}}))
	rows := h - 5 // query, gap, the list, count
	top := max(0, p.sel-rows+1)
	for i := 0; i < rows && top+i < len(p.items); i++ {
		u.drawLine(x0+2, y0+3+i, lw-4, u.pickRow(p.items[top+i], top+i == p.sel))
	}
	count := fmt.Sprintf("%d/%d", len(p.items), len(u.app.Visible(false)))
	switch {
	case p.grep && len(p.items) == grepCap:
		count = fmt.Sprintf("%d+", grepCap)
	case p.grep:
		count = fmt.Sprint(len(p.items))
	}
	u.put(x0+lw-2-len(count), y0+h-2, len(count), count, fg(th.inactive))
	if len(p.items) > 0 {
		u.drawPreview(p.items[p.sel], x0+lw+2, y0+1, w-lw-4, h-2)
	}
	if p.normal {
		u.scr.HideCursor()
		return
	}
	u.scr.SetCursorPosition(x0+4+ansi.StringWidth(string(p.query)), y0+1)
	u.scr.SetCursorStyle(uv.CursorBar, false)
	u.scr.ShowCursor()
}

// box clears a w×h rectangle to the ambient card colour, title in its bottom-right corner.
func (u *UI) box(x, y, w, h int, title string) {
	for r := 0; r < h; r++ {
		u.put(x, y+r, w, strings.Repeat(" ", w), uv.Style{})
	}
	u.put(x+w-2-ansi.StringWidth(title), y+h-1, ansi.StringWidth(title), title, fg(th.subtle))
}

func (u *UI) pickRow(it pickItem, sel bool) line {
	caret, base := "  ", uv.Style{}
	if sel {
		caret, base = "❯ ", uv.Style{Attrs: uv.AttrBold}
	}
	sp := []span{{caret, fg(th.accent)}}
	var tag span
	if u.pick.grep {
		t := ansi.Truncate(cmp.Or(it.s.Title, "New session"), 18, "…")
		sp = append(sp, span{t + strings.Repeat(" ", 19-ansi.StringWidth(t)), fg(th.inactive)}, span{it.who, fg(th.inactive)})
	} else {
		g, gst := u.statusGlyph(it.s)
		sp = append(sp, span{g + " ", gst})
		tag = span{ago(it.s.LastActivityAt), fg(th.inactive)}
	}
	for i, r := range it.text {
		st := base
		if !u.pick.grep && i >= it.dim {
			st = fg(th.inactive)
		}
		if slices.Contains(it.hits, i) {
			st = uv.Style{Fg: th.accent, Attrs: uv.AttrBold}
		}
		if n := len(sp); sp[n-1].st.Equal(&st) {
			sp[n-1].t += string(r)
		} else {
			sp = append(sp, span{string(r), st})
		}
	}
	return line{spans: sp, tag: tag}
}

// drawPreview shows the item's session as its transcript draws it: the newest
// output, or for a grep hit the rows around the match, highlighted.
func (u *UI) drawPreview(it pickItem, x, y, w, h int) {
	u.app.EnsureLoaded(it.s)
	lines := u.transcript(it.s, u.app.L(it.s.SessionID), w)
	sl := screenLines(lines)
	top := max(0, len(lines)-h)
	var pat []rune
	if u.pick.grep {
		pat = u.pick.query
		top = clamp(matchPos(sl, it.entry+1, pat).line-h/3, 0, max(0, len(lines)-h))
	}
	for i := 0; i < h && top+i < len(lines); i++ {
		u.drawLine(x, y+i, w, lines[top+i])
		cells := sl[top+i].cells
		hit := make([]bool, len(cells))
		markMatches(hit, cells, pat)
		u.reverseCells(x, y+i, cells, hit)
	}
}

func (u *UI) statusGlyph(s *core.Session) (string, uv.Style) {
	switch u.app.Status(s) {
	case "running":
		return ember(u.app.L(s.SessionID).Started)
	case "waiting":
		return "?", fg(th.accent)
	case "error":
		return "✗", fg(th.error)
	}
	return " ", uv.Style{}
}

func ago(ms int64) string {
	switch d := time.Since(time.UnixMilli(ms)); {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
