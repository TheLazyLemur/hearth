package tui

import (
	"bytes"
	"strings"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// mdBlocks splits markdown at its top-level blocks so every rendered row can be
// traced back to the raw text it came from. Each chunk keeps the blank lines
// after it, so consecutive chunks concatenate back to the original.
func mdBlocks(src string) []string {
	src = strings.TrimRight(src, "\n")
	b := []byte(src)
	lineStart := func(p int) int { return bytes.LastIndexByte(b[:p], '\n') + 1 }
	var first func(n ast.Node) (int, bool)
	first = func(n ast.Node) (int, bool) {
		if f, ok := n.(*ast.FencedCodeBlock); ok {
			switch {
			case f.Info != nil:
				return f.Info.Segment.Start, true
			case f.Lines().Len() > 0: // the opening fence is the line above the first code line
				return lineStart(max(lineStart(f.Lines().At(0).Start)-1, 0)), true
			}
			return 0, false
		}
		if n.Type() == ast.TypeBlock && n.Lines().Len() > 0 {
			return n.Lines().At(0).Start, true
		}
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if c.Type() == ast.TypeBlock {
				if p, ok := first(c); ok {
					return p, true
				}
			}
		}
		return 0, false // e.g. a thematic break: it stays with the chunk above
	}
	starts := []int{0}
	doc := goldmark.DefaultParser().Parse(text.NewReader(b))
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		if p, ok := first(n); ok && lineStart(p) > starts[len(starts)-1] {
			starts = append(starts, lineStart(p))
		}
	}
	out := make([]string, len(starts))
	for i, s := range starts {
		end := len(src)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		out[i] = src[s:end]
	}
	return out
}

type mdKey struct {
	text  string
	width int
}

// mdDoc is a reply laid out both ways; rows of both point at the same chunk strings.
type mdDoc struct{ rendered, raw []line }

// mdCache keeps laid-out replies so redraws don't re-run glamour, and so chunk
// pointers stay stable between frames for remap.
// shortcut: dropped wholesale when full, make it an LRU if long sessions stutter.
var mdCache = map[mdKey]mdDoc{}

// markdown lays a reply out behind the ⏺ marker: rendered by glamour, or as the
// raw markdown when raw is set.
func markdown(src string, w int, raw bool) []line {
	k := mdKey{src, w}
	d, ok := mdCache[k]
	if !ok {
		chunks := mdBlocks(src)
		d = mdDoc{renderChunks(chunks, w), rawChunks(chunks, w)}
		if len(mdCache) > 500 {
			clear(mdCache)
		}
		mdCache[k] = d
	}
	if raw {
		return d.raw
	}
	return d.rendered
}

// rawChunks wraps each chunk's markdown as typed. The rows carry no source, so
// yanks copy exactly the characters selected.
func rawChunks(chunks []string, w int) []line {
	var out []line
	for i, c := range chunks {
		first := span{"  ", uv.Style{}}
		if i == 0 {
			first = span{"⏺ ", fg(th.text)}
		}
		body := strings.TrimRight(c, "\n")
		rows := wrap([]span{{body, fg(th.text)}}, w, first, nil)
		for range strings.Count(c[len(body):], "\n") - 1 { // blank lines between chunks
			rows = append(rows, line{})
		}
		for j := range rows {
			rows[j].block = &chunks[i]
		}
		out = append(out, rows...)
	}
	return out
}

// renderChunks renders each chunk with glamour. Every row carries the raw chunk
// it was rendered from, so yanks copy markdown, not the rendering.
func renderChunks(chunks []string, w int) []line {
	cfg := styles.DarkStyleConfig
	cfg.Document.BlockPrefix, cfg.Document.BlockSuffix, cfg.Document.Margin = "", "", nil
	r, err := glamour.NewTermRenderer(glamour.WithStyles(cfg), glamour.WithWordWrap(max(w-2, 10)))
	if err != nil {
		panic(err)
	}
	var out []line
	for i, chunk := range chunks {
		raw := &chunks[i]
		rendered, err := r.Render(chunk)
		if err != nil { // glamour only fails on writer errors; show the raw text rather than nothing
			rendered = chunk
		}
		if len(out) > 0 {
			out = append(out, line{source: out[len(out)-1].source, block: out[len(out)-1].block}) // the gap belongs to the chunk above
		}
		var rows [][]span
		for _, row := range uv.NewStyledString(rendered).Lines(ansi.WcWidth) {
			rows = append(rows, cellSpans(row))
		}
		for len(rows) > 0 && len(rows[0]) == 0 { // glamour pads blocks with blank rows; we space chunks ourselves
			rows = rows[1:]
		}
		for len(rows) > 0 && len(rows[len(rows)-1]) == 0 {
			rows = rows[:len(rows)-1]
		}
		for _, sp := range rows {
			out = append(out, line{spans: append([]span{{"  ", uv.Style{}}}, sp...), prefix: 2, source: raw, block: raw})
		}
	}
	if len(out) == 0 {
		out = append(out, line{spans: []span{{"  ", uv.Style{}}}, prefix: 2})
	}
	out[0].spans[0] = span{"⏺ ", fg(th.text)}
	return out
}

// remap finds p's row after replies switch between rendered and raw: the same
// row within the same chunk where it still exists, else the chunk's last row.
// Plain rows read the same both ways, so the nth plain row stays the nth.
func remap(p position, old, cur []screenLine) position {
	if p.line >= len(old) {
		return p
	}
	b, n := old[p.line].block, 0
	for _, l := range old[:p.line] {
		if l.block == b {
			n++
		}
	}
	last := -1
	for j, l := range cur {
		if l.block != b {
			continue
		}
		if n == 0 {
			return position{line: j, col: p.col}
		}
		n, last = n-1, j
	}
	if last < 0 { // the chunk is gone (cache cleared, reply changed): stay put
		return p
	}
	return position{line: last, col: p.col}
}

// cellSpans merges a rendered row's cells into spans, dropping glamour's
// trailing padding so $ and searches see only the text.
func cellSpans(row uv.Line) []span {
	end := len(row)
	for end > 0 && (row[end-1].Content == " " || row[end-1].Content == "") && row[end-1].Style.Bg == nil {
		end--
	}
	var out []span
	for _, c := range row[:end] {
		if c.Content == "" { // the tail of a wide character
			continue
		}
		if n := len(out); n > 0 && out[n-1].st.Equal(&c.Style) {
			out[n-1].t += c.Content
		} else {
			out = append(out, span{c.Content, c.Style})
		}
	}
	return out
}
