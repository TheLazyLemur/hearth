package tui

import (
	"cmp"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/screen"
	"github.com/charmbracelet/x/ansi"

	"hearth/internal/core"
)

var (
	modes   = []string{"default", "acceptEdits", "plan", "auto"}
	efforts = []string{"", "low", "medium", "high", "xhigh", "max"}
)

const (
	paneSidebar = iota
	paneSession
)

const (
	focusInput = iota
	focusView
)

// prompt is the bottom command line: ":" commands or a one-off question.
type prompt struct {
	label    string
	onSubmit func(string)
	choices  func() (cs []choice, loading bool) // rows to pick from; nil for free text only
}

// choice is one row a prompt offers; picking it submits value.
type choice struct{ value, label string }

func matches(c choice, typed string) bool {
	t := strings.ToLower(typed)
	return strings.Contains(strings.ToLower(c.value), t) || strings.Contains(strings.ToLower(c.label), t)
}

// pick resolves typed against cs: a row number, else the first row containing the text, else the text itself.
func pick(cs []choice, typed string) string {
	if n, err := strconv.Atoi(typed); err == nil {
		if n >= 1 && n <= len(cs) {
			return cs[n-1].value
		}
		return typed
	}
	for _, c := range cs {
		if typed != "" && matches(c, typed) {
			return c.value
		}
	}
	return typed
}

// row is one sidebar line: a project header (s == nil) or a session under it.
type row struct {
	project string
	s       *core.Session
}

func (r row) key() string {
	if r.s == nil {
		return "p:" + r.project
	}
	return r.s.SessionID
}

// groupRows groups sessions under their project (originCwd). Projects are ordered by
// their first session in vs, so the project holding the most recent/starred work leads.
func groupRows(vs []*core.Session, collapsed map[string]bool) []row {
	var order []string
	by := map[string][]*core.Session{}
	for _, s := range vs {
		if _, ok := by[s.OriginCwd]; !ok {
			order = append(order, s.OriginCwd)
		}
		by[s.OriginCwd] = append(by[s.OriginCwd], s)
	}
	var rows []row
	for _, p := range order {
		rows = append(rows, row{project: p})
		if !collapsed[p] {
			for _, s := range by[p] {
				rows = append(rows, row{project: p, s: s})
			}
		}
	}
	return rows
}

type UI struct {
	app          *core.App
	scr          *uv.TerminalScreen
	ctx          *screen.Context
	pane         int
	insert       bool   // session pane in INSERT mode
	sel          string // selected sidebar row key
	cur          string // session shown in the main pane
	collapsed    map[string]bool
	focus        int            // session pane: focusInput or focusView
	ed           editor         // the prompt, vim-edited (ported from grimalkin)
	tv           transcriptView // the transcript cursor (ported from grimalkin)
	register     []rune         // yank register shared by the prompt and the transcript
	slines       []screenLine   // transcript as last drawn, for the view's motions
	raw          bool           // replies were last drawn as raw markdown (visual mode)
	input        []rune         // ":" command line / one-off question
	pos          int
	view         int // transcript height at last draw, for paging
	showArchived bool
	prompt       *prompt
	op           string // pending vim operator: g, d, z
	permSel      int    // highlighted permission dialog option
	help         bool   // shortcut hints shown under the footer
	quit         bool
	launched     string      // default folder for new sessions
	pick         *picker     // the open session or grep picker
	ambient      color.Color // background for draws that set none: the sidebar or picker card being painted
	lead         bool        // the last key was a lone space: the start of a picker chord
	jump         int         // entry+1 to put the transcript cursor on at the next draw (grep hits)
}

// Run drives app from the terminal until the user quits. launched is the
// default folder for new sessions.
func Run(app *core.App, launched string) error {
	t := uv.DefaultTerminal()
	scr := t.Screen()
	scr.EnterAltScreen()
	scr.EnableBracketedPaste()
	if err := t.Start(); err != nil {
		return err
	}
	tick := time.NewTicker(120 * time.Millisecond) // spinner frames
	defer tick.Stop()
	u := &UI{app: app, scr: scr, ctx: screen.NewContext(scr), collapsed: map[string]bool{}, launched: launched}
	if vs := app.Visible(false); len(vs) > 0 {
		u.cur, u.sel = vs[0].SessionID, vs[0].SessionID
	}
	defer func() {
		app.CloseAll()
		t.Stop()
	}()
	for !u.quit {
		select {
		case ev, ok := <-t.Events():
			if !ok {
				return nil
			}
			u.event(ev)
		case m := <-app.Events():
			app.Handle(m)
		case <-tick.C:
			if !app.AnyRunning() {
				continue
			}
		}
		u.draw()
	}
	return nil
}

func (u *UI) rows() []row { return groupRows(u.app.Visible(u.showArchived), u.collapsed) }

// selRow returns the selected sidebar row, falling back to the first one.
func (u *UI) selRow() (row, int, []row) {
	rs := u.rows()
	for i, r := range rs {
		if r.key() == u.sel {
			return r, i, rs
		}
	}
	if len(rs) == 0 {
		return row{}, -1, rs
	}
	u.sel = rs[0].key()
	return rs[0], 0, rs
}

// session is the one shown in the main pane.
func (u *UI) session() *core.Session {
	if s := u.app.Find(u.cur); s != nil && s.IsArchived == u.showArchived {
		return s
	}
	if vs := u.app.Visible(u.showArchived); len(vs) > 0 {
		u.cur = vs[0].SessionID
		return vs[0]
	}
	return nil
}

func (u *UI) open(s *core.Session) {
	u.cur, u.sel, u.tv, u.focus = s.SessionID, s.SessionID, transcriptView{}, focusInput
	u.collapsed[s.OriginCwd] = false
}

func (u *UI) ask(label, initial string, f func(string)) {
	u.input = []rune(initial)
	u.pos = len(u.input)
	u.prompt = &prompt{label: label, onSubmit: f}
}

func (u *UI) closePrompt() {
	u.prompt, u.input, u.pos = nil, nil, 0
}

func (u *UI) event(ev uv.Event) {
	switch ev := ev.(type) {
	case uv.WindowSizeEvent:
		u.scr.Resize(ev.Width, ev.Height)
	case uv.PasteEvent:
		text := strings.ReplaceAll(ev.Content, "\r\n", "\n")
		switch {
		case u.pick != nil:
			u.pick.query = append(u.pick.query, []rune(strings.ReplaceAll(text, "\n", " "))...)
			u.refreshPicker()
		case u.prompt != nil:
			u.insertText(text)
		case u.pane == paneSession && u.focus == focusInput:
			u.ed.insertText([]rune(text))
		}
	case uv.KeyPressEvent:
		u.app.Notice = ""
		u.key(ev)
	}
}

func (u *UI) key(k uv.KeyPressEvent) {
	if u.pick != nil {
		u.pickKey(k)
		return
	}
	s := u.session()
	lead := u.lead
	u.lead = false
	if k.MatchString("ctrl+c") {
		if s != nil && u.app.Status(s) == "running" {
			u.app.Interrupt(s)
		} else {
			u.app.Notice = "type :q to quit"
		}
		return
	}
	if u.prompt != nil {
		u.promptKey(k)
		return
	}
	if u.idleNormal(s) && u.leader(k, lead) {
		return
	}
	if s != nil && u.pane == paneSession {
		switch {
		case len(u.app.L(s.SessionID).Perms) > 0 && u.app.L(s.SessionID).Perms[0].Answers == nil:
			u.cardKey(s, k)
		case k.MatchString("shift+tab"):
			u.cycleMode(s)
		case u.focus == focusView:
			u.viewKey(s, k)
		default:
			u.inputKey(s, k)
		}
		return
	}
	// sidebar: two-key vim sequences (gg, dd, za) build up in u.op
	seq := u.op + k.String()
	u.op = ""
	if seq == "g" || seq == "d" || seq == "z" {
		u.op = seq
		return
	}
	switch {
	case seq == ":":
		u.ask(":", "", u.command)
	case seq == "?":
		u.help = !u.help
	case k.MatchString("ctrl+l"):
		if s != nil {
			u.pane = paneSession
		}
	case k.MatchString("shift+tab") && s != nil:
		u.cycleMode(s)
	default:
		u.sidebarKey(seq)
	}
}

func (u *UI) sidebarKey(seq string) {
	r, i, rs := u.selRow()
	if i < 0 {
		if seq == "o" || seq == "O" {
			u.ask(":", "e "+shortPath(u.launched), u.command)
		}
		return
	}
	move := func(j int) {
		j = max(0, min(len(rs)-1, j))
		u.sel = rs[j].key()
		if rs[j].s != nil {
			u.cur, u.tv = rs[j].s.SessionID, transcriptView{}
		}
	}
	switch seq {
	case "j", "down":
		move(i + 1)
	case "k", "up":
		move(i - 1)
	case "gg":
		move(0)
	case "G":
		move(len(rs) - 1)
	case "l", "enter", "right":
		if r.s == nil {
			u.collapsed[r.project] = !u.collapsed[r.project]
		} else {
			u.open(r.s)
			u.pane = paneSession
		}
	case "h", "left":
		if r.s != nil {
			u.sel = "p:" + r.project
		} else {
			u.collapsed[r.project] = true
		}
	case "za":
		u.collapsed[r.project] = !u.collapsed[r.project]
		u.sel = "p:" + r.project
	case "o":
		u.newSessionIn(r.project)
	case "O":
		u.ask(":", "e ", u.command)
	case "A":
		u.showArchived = !u.showArchived
		u.sel = ""
	}
	if r.s == nil {
		return
	}
	switch seq {
	case "r":
		u.ask(":", "rename "+r.s.Title, u.command)
	case "s":
		u.app.ToggleStar(r.s)
	case "a":
		u.app.ToggleArchive(r.s)
	case "dd":
		u.confirmDelete(r.s)
	}
}

// inputKey edits the prompt: grimalkin's vim editor plus the session's own chords.
func (u *UI) inputKey(s *core.Session, k uv.KeyPressEvent) {
	char, key := translateKey(k)
	normal := u.ed.mode == modeNormal
	switch {
	case k.MatchString("ctrl+s"):
		u.submit(s, true)
		return
	case key == keyNewline:
		if !normal {
			u.ed.insertText([]rune("\n"))
		}
		return
	case key == keyPageUp || key == keyCtrlB: // page up from the prompt lands in the transcript
		u.focusView()
		u.viewKey(s, k)
		return
	case normal && char == ':':
		u.ask(":", "", u.command)
		return
	case normal && char == '?':
		u.help = !u.help
		return
	case normal && key == keyEsc:
		u.app.Interrupt(s)
		return
	case normal && k.MatchString("ctrl+h"):
		u.pane = paneSidebar
		return
	}
	switch u.ed.handleKey(&u.register, char, key) {
	case editorSend:
		u.submit(s, false)
	case editorFocusTranscript:
		u.focusView()
	}
}

// viewKey moves the transcript cursor; keys the view leaves free run session commands.
func (u *UI) viewKey(s *core.Session, k uv.KeyPressEvent) {
	char, key := translateKey(k)
	tv := &u.tv
	defer func() {
		if u.focus == focusInput { // left for the prompt: hold the scroll unless it was at the newest output
			tv.held = tv.scrollTop < len(u.slines)-u.view
		}
	}()
	if !tv.search.typing && tv.visual == visualOff && tv.pendingKey == 0 && !tv.pendingWindowCmd {
		op := u.op
		u.op = ""
		switch {
		case key == keyEsc && tv.search.pattern == nil:
			u.app.Interrupt(s)
			return
		case k.MatchString("ctrl+h"):
			u.pane = paneSidebar
			return
		case char == 'd' && op == "d":
			u.confirmDelete(s)
			return
		case char == 'd':
			u.op = "d"
			return
		case char == 'x':
			u.unqueue(s)
			return
		case char == 'm':
			u.cycleMode(s)
			return
		case char == 'M':
			u.pickModel(s)
			return
		case char == 'R':
			u.ask(":", "rewind ", u.command)
			return
		case char == 't':
			u.ask(":", "task 1", u.command)
			return
		}
	}
	act := tv.handleKey(u.slines, u.view, &u.register, char, key)
	if tv.note != "" {
		u.app.Notice, tv.note = tv.note, ""
	}
	switch act {
	case viewInsert:
		u.focus = focusInput
		u.ed.startInsert(len(u.ed.text))
	case viewFocusInput:
		u.focus, u.ed.mode = focusInput, modeNormal
		u.ed.clampCursor()
	case viewYanked:
		u.yanked()
	case viewCommand:
		u.ask(":", "", u.command)
	case viewQuit:
		u.app.Notice = "type :q to quit"
	}
}

func (u *UI) focusView() {
	u.focus = focusView
	if !u.tv.held { // a held transcript keeps its cursor where the view left it
		u.tv.focusLastLine(u.slines, u.view)
	}
}

// submit sends the prompt (queued by the CLI if a turn is running), or answers a question.
func (u *UI) submit(s *core.Session, interrupt bool) {
	l := u.app.L(s.SessionID)
	text := strings.TrimSpace(string(u.ed.text))
	if text == "" {
		return
	}
	var err error
	switch {
	case len(l.Perms) > 0: // AskUserQuestion answer
		err = u.app.AnswerQuestion(s, text)
	case interrupt:
		err = u.app.InterruptAndSend(s, text)
	default:
		err = u.app.Send(s, text)
	}
	if err != nil {
		u.app.Notice = err.Error()
		return
	}
	u.ed.reset()
	u.tv.held = false
}

// pickModel offers s's backend models; typing filters them, a number or enter picks, unlisted text is taken as an id.
func (u *UI) pickModel(s *core.Session) {
	u.ask("model: ", "", func(v string) {
		if v != "" {
			u.app.SetModel(s, v)
		}
	})
	u.prompt.choices = func() ([]choice, bool) {
		ms, loading := u.app.Models(s)
		current := cmp.Or(s.Model, u.app.L(s.SessionID).Model)
		cs := make([]choice, len(ms))
		for i, m := range ms {
			cs[i] = choice{m.ID, strings.Join(slices.DeleteFunc([]string{m.Name, m.Description}, func(x string) bool { return x == "" }), " · ")}
			if m.ID == current {
				cs[i].label += "  (current)"
			}
		}
		return cs, loading
	}
}

// choiceLines lists a prompt's rows that match what is typed (all when it's a row number),
// numbered for picking, with the row enter would take marked.
func (u *UI) choiceLines() []line {
	dim := fg(th.inactive)
	cs, loading := u.prompt.choices()
	if loading {
		return []line{ln(span{"  loading models…", dim})}
	}
	typed := strings.TrimSpace(string(u.input))
	_, err := strconv.Atoi(typed)
	byNumber := err == nil
	picked, marked := pick(cs, typed), false
	var out []line
	for i, c := range cs {
		if !byNumber && !matches(c, typed) {
			continue
		}
		mark, st := "  ", uv.Style{}
		if c.value == picked && !marked {
			mark, st, marked = "❯ ", fg(th.accent), true
		}
		out = append(out, ln(span{mark, st}, span{fmt.Sprintf("%d. ", i+1), dim}, span{c.label, st}))
		if len(out) == 10 {
			break // shortcut: long lists show their first 10 matches; typing narrows them
		}
	}
	if len(out) == 0 {
		out = append(out, ln(span{"  no match · enter uses what you typed as the id", dim}))
	}
	return out
}

func (u *UI) cycleMode(s *core.Session) {
	u.app.SetMode(s, modes[(indexStr(modes, s.PermissionMode)+1)%len(modes)])
}

func (u *UI) unqueue(s *core.Session) {
	u.must(u.app.Unqueue(s, func(text string) {
		u.ed.text, u.focus = []rune(text), focusInput
		u.ed.startInsert(len(u.ed.text))
	}))
}

// yanked copies a transcript yank to the macOS clipboard too, as grimalkin does.
func (u *UI) yanked() {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(string(u.register))
	if err := cmd.Run(); err != nil {
		u.app.Notice = "yanked; clipboard: " + err.Error()
		return
	}
	u.app.Notice = fmt.Sprintf("yanked %d characters", len(u.register))
}

// cardKey drives the permission dialog: j/k or arrows and enter, a number, or esc to deny.
func (u *UI) cardKey(s *core.Session, k uv.KeyPressEvent) {
	opts := permOptions(u.app.L(s.SessionID).Perms[0])
	pick := -1
	switch {
	case k.MatchString("j", "down", "tab"):
		u.permSel = (u.permSel + 1) % len(opts)
	case k.MatchString("k", "up", "shift+tab"):
		u.permSel = (u.permSel + len(opts) - 1) % len(opts)
	case k.MatchString("enter"):
		pick = u.permSel
	case k.MatchString("esc"):
		pick = len(opts) - 1
	case k.MatchString("ctrl+h"):
		u.pane = paneSidebar
	default:
		if n, err := strconv.Atoi(k.Text); err == nil && n >= 1 && n <= len(opts) {
			pick = n - 1
		}
	}
	if pick >= 0 {
		u.permSel = 0
		u.must(u.app.AnswerPerm(s, opts[pick].allow, opts[pick].always))
	}
}

func (u *UI) promptKey(k uv.KeyPressEvent) {
	switch {
	case k.MatchString("enter"):
		p, v := u.prompt, strings.TrimSpace(string(u.input))
		u.closePrompt()
		if p.choices != nil {
			cs, _ := p.choices()
			v = pick(cs, v)
		}
		p.onSubmit(v)
	case k.MatchString("esc"), k.MatchString("backspace") && len(u.input) == 0:
		u.closePrompt()
	default:
		u.edit(k)
	}
}

// command runs a ":" command line.
func (u *UI) command(line string) {
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	s := u.session()
	switch name {
	case "":
		return
	case "q", "q!", "qa", "qa!", "quit", "wq", "x":
		u.quit = true
		return
	case "e", "edit", "new":
		if arg == "" {
			if r, _, _ := u.selRow(); r.project != "" {
				arg = r.project
			} else {
				arg = u.launched
			}
		}
		u.newSessionAt(arg)
		return
	case "archived":
		u.showArchived, u.sel = !u.showArchived, ""
		return
	case "ls":
		u.openPicker(false, "")
		return
	case "grep":
		u.openPicker(true, arg)
		return
	}
	if s == nil {
		u.app.Notice = "no session"
		return
	}
	n, nerr := strconv.Atoi(arg)
	switch name {
	case "rename":
		if arg != "" {
			u.app.Rename(s, arg)
		}
	case "model":
		if arg == "" {
			u.pickModel(s)
			return
		}
		u.app.SetModel(s, arg)
	case "effort":
		valid := u.app.Efforts(s)
		if valid == nil {
			valid = efforts[1:]
		}
		if arg != "" && indexStr(valid, arg) < 0 {
			u.app.Notice = "effort: " + strings.Join(valid, "|")
			return
		}
		u.app.SetEffort(s, arg)
	case "mode":
		if indexStr(modes, arg) < 0 {
			u.app.Notice = "mode: " + strings.Join(modes, "|")
			return
		}
		u.app.SetMode(s, arg)
	case "backend":
		u.must(u.app.SetBackend(s, arg))
	case "rewind":
		if nerr != nil {
			u.app.Notice = "usage: :rewind <turn number>"
			return
		}
		u.rewind(s, n)
	case "task":
		if ns, err := u.app.AcceptChip(s, n-1); err != nil {
			u.app.Notice = err.Error()
		} else {
			u.app.Notice = "started task in background: " + ns.Title
		}
	case "dismiss":
		u.app.DismissChip(s, n-1)
	case "unqueue":
		u.unqueue(s)
	case "star":
		u.app.ToggleStar(s)
	case "archive":
		u.app.ToggleArchive(s)
	case "delete":
		u.confirmDelete(s)
	default:
		u.app.Notice = "not a command: " + name
	}
}

func (u *UI) confirmDelete(s *core.Session) {
	u.ask(fmt.Sprintf("Delete %q? [y/N] ", s.Title), "", func(v string) {
		if strings.EqualFold(v, "y") {
			if err := u.app.Delete(s); err != nil {
				u.app.Notice = err.Error()
			}
		}
	})
}

func (u *UI) newSessionAt(v string) {
	if strings.HasPrefix(v, "~") {
		home, _ := os.UserHomeDir()
		v = home + v[1:]
	}
	abs, err := filepath.Abs(v)
	if st, serr := os.Stat(abs); err != nil || serr != nil || !st.IsDir() {
		u.app.Notice = "not a folder: " + v
		return
	}
	u.newSessionIn(abs)
}

func (u *UI) newSessionIn(dir string) {
	create := func(wt bool) {
		s := u.app.NewSession(dir, wt)
		u.open(s)
		u.pane, u.focus, u.showArchived = paneSession, focusInput, false
		u.ed.startInsert(len(u.ed.text))
	}
	if !u.app.IsRepo(dir) {
		create(false)
		return
	}
	u.ask("Work in an isolated git worktree? [Y/n] ", "", func(v string) { create(!strings.EqualFold(v, "n")) })
}

func (u *UI) rewind(s *core.Session, n int) {
	u.must(u.app.PreviewRewind(s, n, func(r core.RewindPreview) {
		if !r.CanRewind {
			u.app.Notice = "cannot rewind: " + r.Error
			return
		}
		u.ask(fmt.Sprintf("Restore %d file(s) (+%d −%d)? [y/N] ", r.Files, r.Insertions, r.Deletions), "", func(v string) {
			if strings.EqualFold(v, "y") {
				u.must(u.app.Rewind(s, n))
			}
		})
	}))
}

func (u *UI) must(err error) {
	if err != nil {
		u.app.Notice = err.Error()
	}
}

func (u *UI) insertText(s string) {
	r := []rune(s)
	u.input = append(u.input[:u.pos], append(r, u.input[u.pos:]...)...)
	u.pos += len(r)
}

// edit applies vim insert-mode editing keys to the input buffer.
func (u *UI) edit(k uv.KeyPressEvent) {
	switch {
	case k.MatchString("backspace", "ctrl+h"):
		if u.pos > 0 {
			u.input = append(u.input[:u.pos-1], u.input[u.pos:]...)
			u.pos--
		}
	case k.MatchString("ctrl+w"):
		i := u.pos
		for i > 0 && u.input[i-1] == ' ' {
			i--
		}
		for i > 0 && u.input[i-1] != ' ' && u.input[i-1] != '\n' {
			i--
		}
		u.input, u.pos = append(u.input[:i], u.input[u.pos:]...), i
	case k.MatchString("ctrl+u"):
		u.input, u.pos = u.input[u.pos:], 0
	case k.MatchString("left"):
		u.pos = max(0, u.pos-1)
	case k.MatchString("right"):
		u.pos = min(len(u.input), u.pos+1)
	case k.MatchString("home"):
		u.pos = 0
	case k.MatchString("end"):
		u.pos = len(u.input)
	default:
		if k.Text != "" && k.Mod&^uv.ModShift == 0 {
			u.insertText(k.Text)
		}
	}
}

// ---- drawing ----

type span struct {
	t  string
	st uv.Style
}

// line is one screen row of styled spans; bg, when set, fills the rest of the row.
type line struct {
	spans  []span
	bg     color.Color
	prefix int       // leading marker/indent runes, left out of yanks
	brk    lineBreak // how a yank joins this row to the one above
	starts bool      // first row of a transcript entry, for { }
	deco   bool      // not content: yanks skip it
	tag    span      // drawn at the row's right edge, outside the text
	source *string   // raw markdown the row was rendered from: yanks copy it instead
	block  *string   // markdown chunk the row shows, rendered or raw
	entry  int       // index+1 of the Live entry the row draws, 0 for none
}

func ln(spans ...span) line              { return line{spans: spans} }
func fg(c color.Color) uv.Style          { return uv.Style{Fg: c} }
func with(st uv.Style, a uint8) uv.Style { return uv.Style{Fg: st.Fg, Bg: st.Bg, Attrs: st.Attrs | a} }

func (u *UI) put(x, y, w int, text string, st uv.Style) {
	if w <= 0 {
		return
	}
	if st.Bg == nil {
		st.Bg = u.ambient
	}
	u.ctx.SetStyle(st)
	u.ctx.DrawString(ansi.Truncate(strings.ReplaceAll(text, "\t", "    "), w, "…"), x, y)
}

func (u *UI) drawLine(x, y, w int, l line) {
	if l.bg != nil {
		u.put(x, y, w, strings.Repeat(" ", w), uv.Style{Bg: l.bg})
	}
	if n := ansi.StringWidth(l.tag.t); n > 0 {
		u.put(x+w-n, y, n, l.tag.t, l.tag.st)
	}
	for _, s := range l.spans {
		u.put(x, y, w, s.t, s.st)
		n := ansi.StringWidth(s.t)
		x, w = x+n, w-n
	}
}

func width(spans []span) int {
	n := 0
	for _, s := range spans {
		n += ansi.StringWidth(s.t)
	}
	return n
}

// wrap lays spans out in width w. The first row starts with first, later rows with
// blanks as wide as it; rows break at spaces and overlong words are cut.
func wrap(sp []span, w int, first span, bg color.Color) []line {
	ind := span{strings.Repeat(" ", ansi.StringWidth(first.t)), first.st}
	cur, cw := []span{first}, ansi.StringWidth(first.t)
	soft := false // row began at a wrap, so leading spaces are dropped
	var out []line
	brk := breakHard
	flush := func(next lineBreak) {
		if n := len(cur); next == breakAtSpace && n > 0 && strings.HasSuffix(cur[n-1].t, " ") {
			next = breakInWord // the space stayed on the row above
		}
		out = append(out, line{spans: cur, bg: bg, prefix: len([]rune(cur[0].t)), brk: brk})
		cur, cw, soft, brk = []span{ind}, len(ind.t), next != breakHard, next
	}
	for _, s := range sp {
		for i, para := range strings.Split(strings.ReplaceAll(s.t, "\t", "    "), "\n") {
			if i > 0 {
				flush(breakHard)
			}
			for _, word := range splitWords(para) {
				ww := ansi.StringWidth(word)
				if cw+ww > w && cw > len(ind.t) && ww <= w-len(ind.t) { // a word too long for any row is cut in place
					flush(breakAtSpace)
				}
				if word == " " && soft {
					continue
				}
				for cw+ww > w {
					head := ansi.Truncate(word, w-cw, "")
					if head == "" {
						break
					}
					cur = append(cur, span{head, s.st})
					flush(breakInWord)
					word = ansi.TruncateLeft(word, ansi.StringWidth(head), "")
					ww = ansi.StringWidth(word)
				}
				cur, cw, soft = append(cur, span{word, s.st}), cw+ww, false
			}
		}
	}
	return append(out, line{spans: cur, bg: bg, prefix: len([]rune(cur[0].t)), brk: brk})
}

// splitWords splits s into words and single spaces.
func splitWords(s string) []string {
	var out []string
	for s != "" {
		i := strings.IndexByte(s, ' ')
		switch {
		case i < 0:
			out, s = append(out, s), ""
		case i == 0:
			out, s = append(out, " "), s[1:]
		default:
			out, s = append(out, s[:i]), s[i:]
		}
	}
	return out
}

// md styles the markdown the CLI renders inline: headings, **bold**, `code`, fenced blocks.
// shortcut: no syntax highlighting or tables, add if transcripts lean on them.
func md(text string, base uv.Style) []span {
	var out []span
	fence := false
	for i, l := range strings.Split(text, "\n") {
		if i > 0 {
			out = append(out, span{"\n", base})
		}
		switch h := strings.TrimLeft(l, "#"); {
		case strings.HasPrefix(strings.TrimSpace(l), "```"):
			fence = !fence
		case fence:
			out = append(out, span{l, base})
		case h != l && strings.HasPrefix(h, " "):
			out = append(out, inline(h[1:], with(base, uv.AttrBold|uv.AttrItalic))...)
		default:
			out = append(out, inline(l, base)...)
		}
	}
	return out
}

func inline(l string, st uv.Style) []span {
	var out []span
	for {
		i := strings.IndexAny(l, "`*")
		if i < 0 {
			break
		}
		mark, inner := "`", uv.Style{Fg: th.accent, Bg: st.Bg, Attrs: st.Attrs}
		if l[i] == '*' {
			if !strings.HasPrefix(l[i:], "**") {
				out, l = append(out, span{l[:i+1], st}), l[i+1:]
				continue
			}
			mark, inner = "**", with(st, uv.AttrBold)
		}
		j := strings.Index(l[i+len(mark):], mark)
		if j < 0 {
			break
		}
		out = append(out, span{l[:i], st})
		if mark == "**" { // bold may wrap `code`
			out = append(out, inline(l[i+2:i+2+j], inner)...)
		} else {
			out = append(out, span{l[i+1 : i+1+j], inner})
		}
		l = l[i+2*len(mark)+j:]
	}
	return append(out, span{l, st})
}

func (u *UI) modeName() string {
	switch {
	case u.pane != paneSession:
		return "NORMAL"
	case u.focus == focusInput && u.ed.mode == modeInsert:
		return "INSERT"
	case u.focus == focusInput:
		return "NORMAL"
	case u.tv.visual == visualChar:
		return "VISUAL"
	case u.tv.visual == visualLine:
		return "VISUAL LINE"
	}
	return "VIEW"
}

// rawMarkdown shows replies unrendered while selecting, so a selection copies exactly what it covers.
func (u *UI) rawMarkdown() bool {
	return u.pane == paneSession && u.focus == focusView && u.tv.visual != visualOff
}

func (u *UI) searching() bool {
	return u.pane == paneSession && u.focus == focusView && u.tv.search.typing
}

func (u *UI) draw() {
	scr := u.scr
	screen.Clear(scr)
	W, H := scr.Width(), scr.Height()
	if W < 40 || H < 10 {
		u.put(0, 0, W, "terminal too small", fg(th.error))
		scr.Render()
		scr.Flush()
		return
	}
	sw := min(36, W/3)
	u.drawSidebar(sw, H)
	x, w := sw+3, W-sw-5
	scr.HideCursor()
	s := u.session()
	var l *core.Live
	if s != nil {
		u.app.EnsureLoaded(s)
		l = u.app.L(s.SessionID)
	}
	bottom, cy, cx := u.bottom(s, l, w)
	top := H - len(bottom)
	for i, b := range bottom {
		u.drawLine(x, top+i, w, b)
	}
	if cy >= 0 {
		scr.SetCursorPosition(min(x+w-1, x+cx), top+cy)
		scr.ShowCursor()
	}
	shape := uv.CursorBlock
	if u.modeName() == "INSERT" || u.prompt != nil || u.searching() {
		shape = uv.CursorBar
	}
	scr.SetCursorStyle(shape, false)
	if s == nil {
		msg := "no sessions · o to start one, or :e <folder>"
		cy := top/2 - 2
		for i, row := range logo {
			u.drawLine(x+(w-logoW)/2, cy+i, logoW, ln(row...))
		}
		u.put(x+(w-ansi.StringWidth(msg))/2, cy+len(logo)+1, w, msg, fg(th.inactive))
	} else {
		lines := u.transcript(s, l, w)
		old := u.slines
		u.slines, u.view = screenLines(lines), max(1, top-1)
		if raw := u.rawMarkdown(); raw != u.raw { // replies reflowed: keep the cursor on what it was on
			u.raw = raw
			was := u.tv.cursor.line
			u.tv.cursor, u.tv.anchor = remap(u.tv.cursor, old, u.slines), remap(u.tv.anchor, old, u.slines)
			u.tv.scrollTop += u.tv.cursor.line - was
		}
		if u.jump > 0 {
			u.tv.cursor = matchPos(u.slines, u.jump, u.tv.search.pattern)
			u.tv.scrollTop, u.jump = u.tv.cursor.line-u.view/3, 0
		}
		viewing := u.pane == paneSession && u.focus == focusView
		if viewing {
			u.tv.clampAndScroll(u.slines, u.view)
		} else if u.tv.held {
			u.tv.scrollTop = min(u.tv.scrollTop, max(0, len(lines)-u.view))
		} else {
			u.tv.scrollTop = max(0, len(lines)-u.view) // follow the newest output
		}
		start := u.tv.scrollTop
		for i := 0; i < u.view && start+i < len(lines); i++ {
			u.drawLine(x, i, w, lines[start+i])
		}
		if viewing {
			u.drawViewMarks(x, start, cy < 0)
		}
	}
	if u.pick != nil {
		u.drawPicker(W, H)
	}
	scr.Render()
	scr.Flush()
	// a frame that only moves the cursor needs a second pass to show it (grimalkin's present)
	scr.Render()
	scr.Flush()
}

// drawViewMarks reverses the visual selection and search matches, and puts the
// terminal cursor on the transcript cursor.
func (u *UI) drawViewMarks(x, start int, showCursor bool) {
	vs, ve := u.tv.selection()
	pat := u.tv.search.pattern
	for i := 0; i < u.view && start+i < len(u.slines); i++ {
		li, cells := start+i, u.slines[start+i].cells
		hit := make([]bool, len(cells))
		if u.tv.visual != visualOff && li >= vs.line && li <= ve.line {
			from, to := 0, len(cells)-1
			if li == vs.line {
				from = vs.col
			}
			if li == ve.line && ve.col != endOfLine {
				to = min(to, ve.col)
			}
			for c := from; c <= to; c++ {
				hit[c] = true
			}
		}
		markMatches(hit, cells, pat)
		u.reverseCells(x, i, cells, hit)
	}
	if c := u.tv.cursor; showCursor && c.line >= start && c.line < start+u.view && c.line < len(u.slines) {
		cells := u.slines[c.line].cells
		cx := x
		for _, ce := range cells[:min(c.col, len(cells))] {
			cx += ansi.StringWidth(string(ce.char))
		}
		u.scr.SetCursorPosition(cx, c.line-start)
		u.scr.ShowCursor()
	}
}

func markMatches(hit []bool, cells []cell, pat []rune) {
	for _, m := range matchStarts(cells, pat, ignoresCase(pat)) {
		for c := m; c < m+len(pat); c++ {
			hit[c] = true
		}
	}
}

// reverseCells reverses the drawn cells of row y, starting at x, where hit is set.
func (u *UI) reverseCells(x, y int, cells []cell, hit []bool) {
	for c, ce := range cells {
		if cell := u.scr.CellAt(x, y); hit[c] && cell != nil {
			rev := *cell
			rev.Style.Attrs |= uv.AttrReverse
			u.scr.SetCell(x, y, &rev)
		}
		x += ansi.StringWidth(string(ce.char))
	}
}

// screenLines gives the view each row's text and the breaks yanks rejoin it by.
func screenLines(lines []line) []screenLine {
	out := make([]screenLine, len(lines))
	for i, l := range lines {
		sl := screenLine{prefixLen: l.prefix, breakBefore: l.brk, startsEntry: l.starts, decoration: l.deco, source: l.source, block: l.block, entry: l.entry}
		for _, sp := range l.spans {
			for _, r := range sp.t {
				sl.cells = append(sl.cells, cell{r})
			}
		}
		out[i] = sl
	}
	return out
}

func (u *UI) drawSidebar(w, h int) {
	u.ambient = th.panel
	defer func() { u.ambient = nil }()
	for y := 0; y < h; y++ {
		u.put(0, y, w+1, strings.Repeat(" ", w+1), uv.Style{})
	}
	title := "projects"
	if u.showArchived {
		title = "archived"
	}
	u.put(2, 0, w-2, title, fg(th.inactive))
	_, si, rs := u.selRow()
	top := max(0, si-(h-3)) // keep the selection in view
	y := 2
	for i := top; i < len(rs) && y < h; i, y = i+1, y+1 {
		r := rs[i]
		sel := i == si
		bar, st := " ", fg(th.inactive)
		if sel && u.pane == paneSidebar {
			bar, st = "▎", fg(th.text)
		} else if sel {
			bar, st = "▎", uv.Style{}
		}
		u.put(0, y, 1, bar, fg(th.accent))
		if r.s == nil {
			if !sel {
				st = fg(th.inactive)
			}
			fold := "  "
			if u.collapsed[r.project] {
				fold = "+ "
			}
			u.drawLine(2, y, w-2, ln(span{fold, fg(th.subtle)}, span{filepath.Base(r.project), st},
				span{"  " + shortPath(filepath.Dir(r.project)), fg(th.subtle)}))
			continue
		}
		glyph, gst := u.statusGlyph(r.s)
		star := "  "
		if r.s.IsStarred {
			star = "★ "
		}
		t := r.s.Title
		if t == "" {
			t = "New session"
		}
		branch, tag := "", ""
		if sel { // details only on the row you're on
			if r.s.Branch != "" {
				branch = "  " + strings.TrimPrefix(r.s.Branch, "claude/")
			}
			tag = u.app.Backend(r.s).Tag + " " // before the title, which truncation would cut it from
		}
		u.drawLine(4, y, w-4, ln(span{glyph + " ", gst}, span{star, fg(th.warning)}, span{tag, fg(th.subtle)}, span{t, st}, span{branch, fg(th.subtle)}))
	}
}

// header opens the transcript: the hearth, backend, version, model and where the session runs.
func (u *UI) header(s *core.Session, l *core.Live) []line {
	dim := fg(th.inactive)
	ver := ""
	if l.Version != "" {
		ver = " " + l.Version
	}
	model := cmp.Or(s.Model, l.Model, "default model")
	gap := span{"   ", uv.Style{}}
	return []line{
		ln(append(logo[0], gap, span{u.app.Backend(s).Label, fg(th.text)}, span{ver, fg(th.subtle)})...),
		ln(append(logo[1], gap, span{model, dim})...),
		ln(append(logo[2], gap, span{shortPath(s.Cwd) + branchSuffix(s), dim})...),
	}
}

// logo is the hearth: a stone fireplace with a vermilion flame and a warm ember at its heart.
var logo = func() [][]span {
	stone, flame, ember := fg(th.inactive), fg(th.accent), fg(th.warning)
	return [][]span{
		{{" ▟▀▀▀▀▀▙", stone}},
		{{" █ ", stone}, {"▗█▖", flame}, {" █", stone}},
		{{" █", stone}, {"▟█", flame}, {"█", ember}, {"█▙", flame}, {"█", stone}},
	}
}()

const logoW = 8

func (u *UI) transcript(s *core.Session, l *core.Live, w int) []line {
	lines := u.header(s, l)
	for i := range lines {
		lines[i].deco = true
	}
	results, tools := map[string]core.Entry{}, map[string]bool{}
	for _, e := range l.Entries {
		switch {
		case e.ID == "":
		case e.Kind == "result":
			results[e.ID] = e
		case e.Kind == "tool":
			tools[e.ID] = true
		}
	}
	dim := fg(th.inactive)
	turn := 0
	for i, e := range l.Entries {
		if e.Kind == "result" && tools[e.ID] {
			continue // drawn under its tool call
		}
		if e.Kind != "result" {
			lines = append(lines, line{})
		}
		first := len(lines)
		switch e.Kind {
		case "user":
			bg := th.userBg
			ls := wrap(md(e.Text, uv.Style{Fg: th.text, Bg: bg}), w-5, span{"❯ ", uv.Style{Fg: th.inactive, Bg: bg}}, bg)
			if e.UUID != "" { // turn number for :rewind
				turn++
				ls[0].tag = span{fmt.Sprintf("%d ", turn), uv.Style{Fg: th.inactive, Bg: bg}}
			}
			lines = append(lines, ls...)
		case "assistant":
			lines = append(lines, markdown(e.Text, w, u.rawMarkdown())...)
		case "tool":
			r, done := results[e.ID]
			dot := th.inactive
			if done && r.Err {
				dot = th.error
			} else if done {
				dot = th.success
			}
			name, args, _ := strings.Cut(e.Text, "(")
			lines = append(lines, wrap([]span{{name, uv.Style{Attrs: uv.AttrBold}}, {"(" + args, uv.Style{}}}, w, span{"⏺ ", fg(dot)}, nil)...)
			if done {
				lines = append(lines, resultLines(r, w)...)
			}
		case "result":
			lines = append(lines, resultLines(e, w)...)
		case "interrupt":
			lines = append(lines, ln(span{"  ⎿  ", dim}, span{"Interrupted", fg(th.error)}, span{" · What should Claude do instead?", dim}))
		case "error":
			lines = append(lines, wrap([]span{{e.Text, fg(th.error)}}, w, span{"  ⎿  ", dim}, nil)...)
		default:
			lines = append(lines, wrap([]span{{e.Text, dim}}, w, span{}, nil)...)
		}
		if first < len(lines) {
			lines[first].starts = true
		}
		for j := first; j < len(lines); j++ {
			lines[j].entry = i + 1
		}
	}
	if l.Running && len(l.Perms) == 0 {
		dot, cl := ember(l.Started)
		spin := ln(span{dot + " ", cl}, span{strings.ToLower(l.Verb) + "…", fg(th.text)},
			span{" (" + core.FormatDuration(time.Since(l.Started)) + " · ctrl+c to interrupt)", dim})
		spin.deco = true
		lines = append(lines, line{}, spin)
	}
	return lines
}

func resultLines(e core.Entry, w int) []line {
	st := uv.Style{}
	if e.Err {
		st = fg(th.error)
	}
	text := e.Text
	if text == "" {
		text = "(No content)"
	}
	return wrap([]span{{text, st}}, w, span{"  ⎿  ", fg(th.inactive)}, nil)
}

// bottom builds everything under the transcript: task chips, queued messages, the
// permission dialog or prompt box, and the footer. It returns the cursor row and
// column within those lines, row -1 when the cursor is hidden.
func (u *UI) bottom(s *core.Session, l *core.Live, w int) ([]line, int, int) {
	dim := fg(th.inactive)
	var out []line
	cy, cx := -1, 0
	if s != nil {
		for i, c := range l.Chips {
			out = append(out, wrap([]span{{c.Title + " — " + c.Tldr, uv.Style{}}}, w, span{fmt.Sprintf("◆ task %d  ", i+1), fg(th.inactive)}, nil)...)
		}
		if len(l.Chips) > 0 {
			out = append(out, ln(span{"  :task <n> to start · :dismiss <n>", dim}))
		}
		for _, q := range l.Queued {
			out = append(out, wrap([]span{{q.Text, dim}}, w, span{"❯ ", dim}, nil)...)
		}
		if len(l.Queued) > 0 {
			hint := "  queued · " + u.app.Backend(s).Label + " reads it at its next step"
			if u.app.Backend(s).Unqueue {
				hint += " · x pulls the last one back"
			}
			out = append(out, ln(span{hint, dim}))
		}
		if len(l.Perms) > 0 {
			out = append(out, u.permLines(l.Perms[0], w)...)
		}
	}
	if s != nil && (len(l.Perms) == 0 || l.Perms[0].Answers != nil) {
		if s.Effort != "" {
			e := effortGlyphs[s.Effort] + " " + s.Effort + " · :effort"
			out = append(out, ln(span{strings.Repeat(" ", max(0, w-ansi.StringWidth(e))), uv.Style{}}, span{e, dim}))
		}
		rule := ln(span{strings.Repeat("─", w), fg(th.subtle)})
		out = append(out, rule)
		text := u.ed.text
		caret := uv.Style{}
		if l.Running {
			caret = dim
		}
		// at most 6 rows show; the window follows the cursor
		rows := inputRows(text, w-2)
		x, y := cursorXY(text[:min(u.ed.cursor, len(text))], w-2)
		u.ed.scrollRow = min(clamp(u.ed.scrollRow, max(0, y-5), y), max(0, len(rows)-6))
		rows = rows[u.ed.scrollRow:min(len(rows), u.ed.scrollRow+6)]
		if u.pane == paneSession && u.focus == focusInput && u.prompt == nil {
			cy, cx = len(out)+y-u.ed.scrollRow, 2+x
		}
		for i, r := range rows {
			p := "  "
			if i == 0 && u.ed.scrollRow == 0 {
				p = "❯ "
			}
			st := uv.Style{}
			if len(text) == 0 {
				r, st = `Try "how does <filepath> work?"`, uv.Style{Attrs: uv.AttrFaint}
			}
			out = append(out, ln(span{p, caret}, span{r, st}))
		}
		out = append(out, rule)
	}
	if u.prompt != nil && u.prompt.choices != nil {
		out = append(out, u.choiceLines()...)
	}
	foot := u.footer(s, w)
	switch {
	case u.prompt != nil:
		cy, cx = len(out), 2+ansi.StringWidth(u.prompt.label+string(u.input[:u.pos]))
	case u.searching():
		cy, cx = len(out), 3+ansi.StringWidth(string(u.tv.search.typed))
	}
	return append(out, foot...), cy, cx
}

// inputRows hard-wraps the prompt the same way cursorXY counts, so the cursor lands on its character.
func inputRows(text []rune, w int) []string {
	rows, cur, x := []string{}, []rune{}, 0
	for _, r := range text {
		rw := ansi.StringWidth(string(r))
		if r == '\n' || x+rw > w {
			rows, cur, x = append(rows, string(cur)), nil, 0
			if r == '\n' {
				continue
			}
		}
		cur, x = append(cur, r), x+rw
	}
	return append(rows, string(cur))
}

func (u *UI) footer(s *core.Session, w int) []line {
	dim := fg(th.inactive)
	switch {
	case u.prompt != nil:
		return []line{ln(span{"  " + u.prompt.label + string(u.input), uv.Style{Attrs: uv.AttrBold}})}
	case u.searching():
		dir := "/"
		if u.tv.search.typedBackward {
			dir = "?"
		}
		return []line{ln(span{"  " + dir + string(u.tv.search.typed), uv.Style{Attrs: uv.AttrBold}})}
	}
	vim := strings.ToLower(u.modeName())
	left := []span{{"  ", uv.Style{}}, {vim, fg(th.subtle)}}
	if s != nil {
		switch s.PermissionMode { // manual is the default and says nothing
		case "acceptEdits":
			left = append(left, span{" · accept edits", fg(th.accent)})
		case "plan":
			left = append(left, span{" · plan", fg(th.accent)})
		case "auto":
			left = append(left, span{" · auto", fg(th.accent)})
		}
	}
	if vim == "normal" {
		left = append(left, span{" · ?", fg(th.subtle)})
	}
	out := []line{ln(left...)}
	if n := u.app.Notice; n != "" {
		if pad := w - width(left) - ansi.StringWidth(n); pad >= 2 {
			out[0].spans = append(out[0].spans, span{strings.Repeat(" ", pad), uv.Style{}}, span{n, fg(th.warning)})
		} else {
			out = append(out, wrap([]span{{n, fg(th.warning)}}, w, span{"  ", uv.Style{}}, nil)...)
		}
	}
	if u.help {
		help := "j/k move · l open · h project · za fold · o new · r rename · s star · a archive · dd delete · A archived · space space sessions · space / grep · :q quit"
		if u.pane == paneSession {
			help = "INSERT  enter send/queue · ctrl+s interrupt+send · ctrl+j newline · esc → NORMAL\n" +
				"NORMAL  h j k l w b e 0 ^ $ · d c y + motion, dd cc yy · x X r D C · o O p P u · enter send · i a I A · k on top line → VIEW · esc interrupt\n" +
				"VIEW    h j k l w b e 0 $ gg G · ^d ^u ^f ^b · { } · / ? n N · v V o y yy y$ · j past end → NORMAL · i insert · " +
				"x unqueue · m mode · M model · R rewind · t task · dd delete · esc interrupt\n" +
				"        space space sessions · space / grep · ctrl+h list · :backend :effort :model :mode :rename :rewind :task · ctrl+c interrupt · :q quit"
		}
		out = append(out, wrap([]span{{help, dim}}, w, span{"  ", uv.Style{}}, nil)...)
	}
	return out
}

func branchSuffix(s *core.Session) string {
	if s.Branch != "" {
		return " ⎇ " + s.Branch
	}
	return ""
}

// permOption is one choice in the permission dialog.
type permOption struct {
	label         string
	allow, always bool
}

func permOptions(p *core.Perm) []permOption {
	opts := []permOption{{"Yes", true, false}}
	if len(p.Suggestions) > 0 {
		opts = append(opts, permOption{alwaysLabel(p.Suggestions[0]), true, true})
	}
	return append(opts, permOption{"No", false, false})
}

// alwaysLabel words the CLI's "don't ask again" choice for a permission suggestion.
func alwaysLabel(sug any) string {
	m, _ := sug.(map[string]any)
	switch m["type"] {
	case "addRules":
		var rules []string
		rs, _ := m["rules"].([]any)
		for _, r := range rs {
			rm, _ := r.(map[string]any)
			rules = append(rules, cmp.Or(str(rm["ruleContent"]), str(rm["toolName"])))
		}
		return "Yes, and don’t ask again for: " + strings.Join(rules, ", ")
	case "setMode":
		if m["mode"] == "acceptEdits" {
			return "Yes, allow all edits during this session (shift+tab)"
		}
	}
	return "Yes, and don’t ask again"
}

// permLines draws the CLI-style permission dialog, or an AskUserQuestion card.
func (u *UI) permLines(p *core.Perm, w int) []line {
	dim, perm := fg(th.inactive), fg(th.accent)
	out := []line{ln(span{strings.Repeat("─", w), perm})}
	if p.Answers != nil {
		qs := p.Questions()
		if len(p.Answers) >= len(qs) {
			return nil
		}
		q := qs[len(p.Answers)]
		out = append(out, wrap([]span{{q.Question, uv.Style{Attrs: uv.AttrBold}}}, w, span{fmt.Sprintf(" (%d/%d) ", len(p.Answers)+1, len(qs)), dim}, nil)...)
		for i, o := range q.Options {
			out = append(out, ln(span{fmt.Sprintf("   %d. ", i+1), dim}, span{o, uv.Style{}}))
		}
		return append(out, line{}, ln(span{" i, then type a number or your own answer · enter to submit", dim}))
	}
	title, body := p.Tool, core.SummariseInput(p.Input)
	switch p.Tool {
	case "Bash":
		title, body = "Bash command", core.FirstLines(str(p.Input["command"]), 8)
	case "Edit", "MultiEdit", "NotebookEdit":
		title = "Edit file"
	case "Write":
		title = "Create file"
	case "Read":
		title = "Read file"
	case "ExitPlanMode":
		title, body = "Ready to code?", core.FirstLines(str(p.Input["plan"]), 12)
	}
	out = append(out, ln(span{" " + title, with(perm, uv.AttrBold)}))
	if d := str(p.Input["description"]); d != "" {
		out = append(out, wrap([]span{{d, dim}}, w, span{" ", uv.Style{}}, nil)...)
	}
	dash := ln(span{strings.Repeat("╌", w), fg(th.subtle)})
	out = append(out, dash)
	out = append(out, wrap([]span{{body, uv.Style{}}}, w, span{" ", uv.Style{}}, nil)...)
	out = append(out, dash)
	if p.Reason != "" {
		out = append(out, wrap([]span{{"Flagged by auto mode: " + p.Reason, fg(th.warning)}}, w, span{" ", uv.Style{}}, nil)...)
	}
	out = append(out, line{}, ln(span{" Do you want to proceed?", uv.Style{}}))
	for i, o := range permOptions(p) {
		caret, st := "   ", uv.Style{}
		if i == u.permSel {
			caret, st = " ❯ ", perm
		}
		out = append(out, ln(span{caret, perm}, span{fmt.Sprintf("%d. ", i+1), dim}, span{o.label, st}))
	}
	return append(out, line{}, ln(span{" Esc to cancel", dim}))
}

// cursorXY maps a cursor offset to a position inside the wrapped input box.
func cursorXY(before []rune, w int) (int, int) {
	x, y := 0, 0
	for _, r := range before {
		if r == '\n' {
			x, y = 0, y+1
			continue
		}
		rw := ansi.StringWidth(string(r))
		if x+rw > w {
			x, y = 0, y+1
		}
		x += rw
	}
	return x, y
}

func shortPath(p string) string {
	if home, _ := os.UserHomeDir(); home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

func str(v any) string { s, _ := v.(string); return s }

func indexStr(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}
