package tui

import (
	"encoding/json"
	"image/color"
	"os"
)

// theme is the subset of Claude Code's palette this TUI draws with, copied from the
// CLI binary's theme tables (dark and dark-daltonized).
type theme struct {
	claude, permission, planMode, autoAccept, promptBorder, text, inactive, subtle,
	suggestion, success, error, warning, userBg color.Color
}

func rgb(r, g, b uint8) color.Color { return color.RGBA{r, g, b, 255} }

var themes = map[string]theme{
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
	if t, ok := themes[cfg.Theme]; ok {
		return t
	}
	return themes["dark"]
}

// Spinner glyphs, ping-ponged, and the effort level markers.
var (
	spinnerFrames = []string{"·", "✢", "✳", "✶", "✻", "✽"}
	effortGlyphs  = map[string]string{"low": "○", "medium": "◐", "high": "●", "xhigh": "◉", "max": "◈"}
)
