package tui

import (
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"

	"claude-code-tui/internal/core"
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
