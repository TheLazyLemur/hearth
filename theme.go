package main

import (
	"encoding/json"
	"image/color"
	"os"
	"path/filepath"
)

// theme is the subset of Claude Code's palette this TUI draws with, copied from the
// CLI binary's theme tables (dark and dark-daltonized).
type theme struct {
	claude, permission, planMode, autoAccept, promptBorder, text, inactive, subtle,
	suggestion, success, error, warning, userBg, logoBg color.Color
}

func rgb(r, g, b uint8) color.Color { return color.RGBA{r, g, b, 255} }

var themes = map[string]theme{
	"dark": {claude: rgb(215, 119, 87), permission: rgb(177, 185, 249), planMode: rgb(72, 150, 140),
		autoAccept: rgb(175, 135, 255), promptBorder: rgb(136, 136, 136), text: rgb(255, 255, 255),
		inactive: rgb(153, 153, 153), subtle: rgb(80, 80, 80), suggestion: rgb(177, 185, 249),
		success: rgb(78, 186, 101), error: rgb(255, 107, 128), warning: rgb(255, 193, 7),
		userBg: rgb(55, 55, 55), logoBg: rgb(0, 0, 0)},
	"dark-daltonized": {claude: rgb(255, 153, 51), permission: rgb(153, 204, 255), planMode: rgb(102, 153, 153),
		autoAccept: rgb(175, 135, 255), promptBorder: rgb(136, 136, 136), text: rgb(255, 255, 255),
		inactive: rgb(153, 153, 153), subtle: rgb(80, 80, 80), suggestion: rgb(153, 204, 255),
		success: rgb(51, 153, 255), error: rgb(255, 102, 102), warning: rgb(255, 204, 0),
		userBg: rgb(55, 55, 55), logoBg: rgb(0, 0, 0)},
}

// loadTheme follows the user's Claude Code "theme" setting in ~/.claude.json.
// shortcut: light and ansi themes fall back to dark, add their tables if anyone runs a light terminal.
func loadTheme() theme {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	path := filepath.Join(dir, ".claude.json")
	if dir == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".claude.json")
	}
	var cfg struct{ Theme string }
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &cfg)
	}
	if t, ok := themes[cfg.Theme]; ok {
		return t
	}
	return themes["dark"]
}

// Spinner glyphs, ping-ponged, and the verbs the CLI shows while and after a turn runs.
var (
	spinnerFrames = []string{"·", "✢", "✳", "✶", "✻", "✽"}
	doneVerbs     = []string{"Baked", "Brewed", "Churned", "Cogitated", "Cooked", "Crunched", "Sautéed", "Worked"}
	effortGlyphs  = map[string]string{"low": "○", "medium": "◐", "high": "●", "xhigh": "◉", "max": "◈"}
	spinnerVerbs  = []string{
		"Accomplishing", "Actioning", "Actualizing", "Architecting", "Baking", "Beaming", "Beboppin'",
		"Befuddling", "Billowing", "Blanching", "Bloviating", "Boogieing", "Boondoggling", "Booping",
		"Bootstrapping", "Brewing", "Bunning", "Burrowing", "Calculating", "Canoodling", "Caramelizing",
		"Cascading", "Catapulting", "Cerebrating", "Channeling", "Choreographing", "Churning", "Clauding",
		"Coalescing", "Cogitating", "Combobulating", "Composing", "Computing", "Concocting",
		"Considering", "Contemplating", "Cooking", "Crafting", "Creating", "Crunching", "Crystallizing",
		"Cultivating", "Deciphering", "Deliberating", "Determining", "Dilly-dallying", "Discombobulating",
		"Doing", "Doodling", "Drizzling", "Ebbing", "Effecting", "Elucidating", "Embellishing",
		"Enchanting", "Envisioning", "Fermenting", "Fiddle-faddling", "Finagling", "Flambéing",
		"Flibbertigibbeting", "Flowing", "Flummoxing", "Fluttering", "Forging", "Forming", "Frolicking",
		"Frosting", "Gallivanting", "Galloping", "Garnishing", "Generating", "Gesticulating",
		"Germinating", "Gitifying", "Grooving", "Gusting", "Harmonizing", "Hashing", "Hatching",
		"Herding", "Honking", "Hullaballooing", "Hyperspacing", "Ideating", "Imagining", "Improvising",
		"Incubating", "Inferring", "Infusing", "Ionizing", "Jitterbugging", "Julienning", "Kerfuffling",
		"Kneading", "Leavening", "Levitating", "Lollygagging", "Manifesting", "Marinating", "Meandering",
		"Metamorphosing", "Misting", "Moonwalking", "Moseying", "Mulling", "Mustering", "Nebulizing",
		"Nesting", "Newspapering", "Noodling", "Nucleating", "Onioning", "Orbiting", "Orchestrating",
		"Osmosing", "Perambulating", "Percolating", "Perusing", "Philosophizing", "Photosynthesizing",
		"Polishing", "Pollinating", "Pondering", "Pontificating", "Pouncing", "Precipitating",
		"Prestidigitating", "Processing", "Proofing", "Propagating", "Puttering", "Puzzling",
		"Quantumizing", "Razzle-dazzling", "Razzmatazzing", "Recombobulating", "Reticulating", "Roosting",
		"Ruminating", "Sautéing", "Scampering", "Schlepping", "Scurrying", "Seasoning", "Shenaniganing",
		"Shimmying", "Simmering", "Skedaddling", "Sketching", "Slithering", "Smooshing", "Sock-hopping",
		"Spelunking", "Spinning", "Sprouting", "Stewing", "Sublimating", "Swirling", "Swooping",
		"Symbioting", "Synthesizing", "Tempering", "Thinking", "Thundering", "Tinkering", "Tomfoolering",
		"Topsy-turvying", "Transfiguring", "Transmogrifying", "Transmuting", "Twisting", "Undulating",
		"Unfurling", "Unraveling", "Vibing", "Waddling", "Wandering", "Warping", "Whatchamacalliting",
		"Whirlpooling", "Whirring", "Whisking", "Wibbling", "Working", "Wrangling", "Zesting",
		"Zigzagging",
	}
)
