package tui

import (
	"encoding/json"
	"image/color"
	"math"
	"os"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// theme is the subset of Claude Code's palette this TUI draws with, copied from the
// CLI binary's theme tables (dark and dark-daltonized).
type theme struct {
	claude, permission, planMode, autoAccept, promptBorder, text, inactive, subtle,
	suggestion, success, error, warning, userBg color.Color
	panel, card color.Color // sidebar and floating-card tints; nil draws on the terminal's own background
}

func rgb(r, g, b uint8) color.Color { return color.RGBA{r, g, b, 255} }

var themes = map[string]theme{
	// sumi: ink greys, washi text, one shu (vermilion) accent.
	"sumi": {claude: rgb(206, 78, 54), permission: rgb(206, 78, 54), planMode: rgb(206, 78, 54),
		autoAccept: rgb(206, 78, 54), promptBorder: rgb(58, 56, 53), text: rgb(228, 222, 210),
		inactive: rgb(122, 118, 111), subtle: rgb(58, 56, 53), suggestion: rgb(122, 118, 111),
		success: rgb(128, 146, 108), error: rgb(196, 92, 80), warning: rgb(178, 150, 106),
		userBg: rgb(38, 37, 35), panel: rgb(24, 23, 22), card: rgb(30, 29, 27)},
	"dark": {claude: rgb(215, 119, 87), permission: rgb(177, 185, 249), planMode: rgb(72, 150, 140),
		autoAccept: rgb(175, 135, 255), promptBorder: rgb(136, 136, 136), text: rgb(255, 255, 255),
		inactive: rgb(153, 153, 153), subtle: rgb(80, 80, 80), suggestion: rgb(177, 185, 249),
		success: rgb(78, 186, 101), error: rgb(255, 107, 128), warning: rgb(255, 193, 7),
		userBg: rgb(55, 55, 55)},
	"dark-daltonized": {claude: rgb(255, 153, 51), permission: rgb(153, 204, 255), planMode: rgb(102, 153, 153),
		autoAccept: rgb(175, 135, 255), promptBorder: rgb(136, 136, 136), text: rgb(255, 255, 255),
		inactive: rgb(153, 153, 153), subtle: rgb(80, 80, 80), suggestion: rgb(153, 204, 255),
		success: rgb(51, 153, 255), error: rgb(255, 102, 102), warning: rgb(255, 204, 0),
		userBg: rgb(55, 55, 55)},
}

// loadTheme follows the user's Claude Code "theme" setting in path (~/.claude.json).
// shortcut: light and ansi themes fall back to dark, add their tables if anyone runs a light terminal.
func loadTheme(path string) theme {
	var cfg struct{ Theme string }
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &cfg)
	}
	// shortcut: sumi replaces plain dark (daltonized keeps its own), add a hearth theme setting if both should stay.
	if t, ok := themes[cfg.Theme]; ok && cfg.Theme != "dark" {
		return t
	}
	return themes["sumi"]
}

// effortGlyphs mark the effort level.
var effortGlyphs = map[string]string{"low": "○", "medium": "◐", "high": "●", "xhigh": "◉", "max": "◈"}

// ember is the running marker: one dot whose colour breathes ink → accent → ink every two seconds.
func ember(start time.Time) (string, uv.Style) {
	t := (1 - math.Cos(2*math.Pi*time.Since(start).Seconds()/2)) / 2
	return "●", fg(lerp(th.subtle, th.claude, t))
}

func lerp(a, b color.Color, t float64) color.Color {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	mix := func(x, y uint32) uint8 { return uint8((float64(x>>8)*(1-t) + float64(y>>8)*t) + 0.5) }
	return rgb(mix(ar, br), mix(ag, bg), mix(ab, bb))
}
