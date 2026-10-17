package tui

import (
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"

	"hearth/internal/core"
)

func TestSidebarGroupsSessionsUnderProjects(t *testing.T) {
	// given
	// ... sessions from two projects in recency order, with project b collapsed
	vs := []*core.Session{{SessionID: "local_1", OriginCwd: "/a"}, {SessionID: "local_2", OriginCwd: "/b"}, {SessionID: "local_3", OriginCwd: "/a"}}

	// when
	// ... sidebar rows are built
	rows := groupRows(vs, map[string]bool{"/b": true})

	// then
	// ... project a leads with both its sessions, project b shows only its header
	var got []string
	for _, r := range rows {
		got = append(got, r.key())
	}
	if strings.Join(got, ",") != "p:/a,local_1,local_3,p:/b" {
		t.Fatalf("rows %v", got)
	}
}

func TestInlineMarkdownWrapsUnderItsMarker(t *testing.T) {
	// given
	// ... a reply with a heading, inline code and bold text, in a 14-column pane
	text := "# Title\nsee `go.mod` for **deps** ok"

	// when
	// ... it is styled and wrapped behind the ⏺ marker
	rows := wrap(md(text, uv.Style{}), 14, span{"⏺ ", uv.Style{}}, nil)

	// then
	// ... markers are stripped, rows break at spaces under a hanging indent, code and bold keep their styles
	var got []string
	styles := map[string]uv.Style{}
	for _, r := range rows {
		var b strings.Builder
		for _, s := range r.spans {
			b.WriteString(s.t)
			styles[s.t] = s.st
		}
		got = append(got, b.String())
	}
	if strings.Join(got, "|") != "⏺ Title|  see go.mod |  for deps ok" {
		t.Fatalf("rows %q", got)
	}
	if styles["go.mod"].Fg != th.permission || styles["deps"].Attrs != uv.AttrBold || styles["Title"].Attrs != uv.AttrBold|uv.AttrItalic {
		t.Fatalf("styles %+v", styles)
	}
}

func TestTranscriptYankRejoinsWrappedRowsWithoutMarkers(t *testing.T) {
	// given
	// ... a reply wrapped behind its ⏺ marker, one row ending in the space it broke at, then a second paragraph
	lines := screenLines(wrap(md("alpha bet gamma\nnext", uv.Style{}), 12, span{"⏺ ", uv.Style{}}, nil))
	v := &transcriptView{}
	var register []rune

	// when
	// ... all three rows are yanked linewise in the transcript view
	press(v, lines, 10, &register, "Vjjy")

	// then
	// ... the wrap adds no extra space, the paragraph break is a newline, and the marker is left out
	if len(lines) != 3 || string(register) != "alpha bet gamma\nnext" {
		t.Fatalf("rows %d register %q", len(lines), string(register))
	}
}

func TestRenderedReplyYanksAsRawMarkdown(t *testing.T) {
	// given
	// ... a reply rendered by glamour, so its rows show no markdown markup
	src := "# Title\n\nSome **bold** and `code` text.\n\n- a\n- b\n\nEnd."
	rows := markdown(src, 40, false)
	lines := screenLines(rows)
	v := &transcriptView{}
	var register []rune

	// when
	// ... the rows from the paragraph through the list are yanked linewise
	press(v, lines, 20, &register, "jjVjjjy")

	// then
	// ... the register holds those blocks' raw markdown, not the rendering
	var shown strings.Builder
	for _, sp := range rows[2].spans {
		shown.WriteString(sp.t)
	}
	if s := shown.String(); s != "  Some bold and \u00a0code\u00a0 text." {
		t.Fatalf("rendered row shows markup: %q", s)
	}
	if string(register) != "Some **bold** and `code` text.\n\n- a\n- b" {
		t.Fatalf("register %q", string(register))
	}
}

func TestVisualModeSelectsRawMarkdownWhereTheCursorWas(t *testing.T) {
	// given
	// ... a reply whose code fences make the raw layout taller, the cursor on the rendered "b" of the last item
	src := "```\nx := 1\n```\n\n- a\n- **b** c"
	rendered, raw := screenLines(markdown(src, 40, false)), screenLines(markdown(src, 40, true))
	v := &transcriptView{cursor: position{line: 3, col: 4}}
	var register []rune

	// when
	// ... the view switches to raw markdown and the rest of the line is selected and yanked
	v.cursor = remap(v.cursor, rendered, raw)
	press(v, raw, 20, &register, "v$y")

	// then
	// ... the cursor stayed on that item, now at its raw markup, and the yank holds exactly the raw characters
	if string(register) != "**b** c" {
		t.Fatalf("register %q", string(register))
	}
}

func TestSessionPickerRanksWordStartMatchesFirst(t *testing.T) {
	// given
	// ... sessions in recency order: a scattered match, a non-match, then a word-start match
	vs := []*core.Session{
		{SessionID: "local_1", Title: "rename markdown", OriginCwd: "/p/app"},
		{SessionID: "local_2", Title: "docs", OriginCwd: "/p/app"},
		{SessionID: "local_3", Title: "Refactor kit", OriginCwd: "/p/app"},
	}

	// when
	// ... the picker query is "rk"
	items := sessionItems(vs, []rune("rk"))

	// then
	// ... the non-match is dropped and the word-start match leads despite being older
	var got []string
	for _, it := range items {
		got = append(got, it.s.SessionID)
	}
	if strings.Join(got, ",") != "local_3,local_1" {
		t.Fatalf("order %v", got)
	}
}

type fakeStore struct{ ss []*core.Session }

func (f fakeStore) Load() ([]*core.Session, error) { return f.ss, nil }
func (fakeStore) Save(*core.Session) error         { return nil }
func (fakeStore) Delete(string) error              { return nil }

type fakeTranscripts map[string][]core.Entry

func (f fakeTranscripts) Load(_, id string) ([]core.Entry, error) { return f[id], nil }

func TestGrepHitOpensTheSessionWithTheCursorOnTheMatch(t *testing.T) {
	// given
	// ... a session whose tool output and reply both mention glamour, deep enough to need scrolling
	s := &core.Session{SessionID: "local_1", CLISessionID: "c1", Title: "render", OriginCwd: "/p"}
	es := []core.Entry{{Kind: "user", Text: "add markdown", UUID: "u1"}, {Kind: "tool", Text: "Bash(grep glamour)", ID: "t1"},
		{Kind: "result", Text: "glamour v2", ID: "t1"}, {Kind: "assistant", Text: strings.Repeat("filler\n\n", 5) + "Rendered with **glamour** now."}}
	app, err := core.NewApp(fakeStore{[]*core.Session{s}},
		map[string]core.Backend{core.DefaultBackend: {Label: "Claude Code", Transcripts: fakeTranscripts{"c1": es}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := &UI{app: app, collapsed: map[string]bool{}}

	// when
	// ... "glamour" is grepped and the only hit opened, then the transcript is laid out as draw does
	u.openPicker(true, "glamour")
	items := u.pick.items
	u.pickKey(uv.KeyPressEvent{Code: uv.KeyEnter})
	lines := screenLines(u.transcript(s, app.L(s.SessionID), 60))
	cur := matchPos(lines, u.jump, u.tv.search.pattern)

	// then
	// ... tool output is skipped, the reply's line is the hit, and the cursor sits on "glamour" in the rendered reply
	if len(items) != 1 || string(items[0].text) != "Rendered with **glamour** now." || items[0].who != "⏺ " {
		t.Fatalf("items %+v", items)
	}
	var at []rune
	for _, c := range lines[cur.line].cells[cur.col:min(cur.col+7, len(lines[cur.line].cells))] {
		at = append(at, c.char)
	}
	if u.focus != focusView || string(at) != "glamour" {
		t.Fatalf("focus %d cursor %+v", u.focus, cur)
	}
}

func TestTypedInputPicksAChoice(t *testing.T) {
	cs := []choice{{"opus", "Opus · Opus 5.5"}, {"ollama/deepseek-v4.1-flash:cloud", "DeepSeek V4.1 Flash · ollama"}, {"haiku", "Haiku · Fastest"}}
	cases := []struct{ name, typed, want string }{
		{"number picks that row", "3", "haiku"},
		{"text picks the first row containing it", "deepseek", "ollama/deepseek-v4.1-flash:cloud"},
		{"text matches labels case-insensitively", "FAST", "haiku"},
		{"unmatched text is taken as typed", "anthropic/claude-sonnet-5-5", "anthropic/claude-sonnet-5-5"},
		{"out of range number is taken as typed", "9", "9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// given
			// ... a model list
			list := cs

			// when
			// ... the user submits typed text
			got := pick(list, c.typed)

			// then
			// ... it resolves to the matching choice's value
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}
