package tui

import (
	"image/color"
	"math"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// th is hearth's palette: cool blue-black inks that sit on a near-black terminal, one shu (vermilion) accent.
var th = struct {
	accent, text, inactive, subtle, success, error, warning, userBg color.Color
	panel                                                           color.Color // sidebar and picker-card tint
}{
	accent: rgb(206, 78, 54), text: rgb(214, 222, 232), inactive: rgb(104, 114, 134), subtle: rgb(46, 52, 68),
	success: rgb(128, 146, 108), error: rgb(196, 92, 80), warning: rgb(178, 150, 106),
	userBg: rgb(22, 27, 40), panel: rgb(11, 14, 22),
}

func rgb(r, g, b uint8) color.Color { return color.RGBA{r, g, b, 255} }

// effortGlyphs mark the effort level.
var effortGlyphs = map[string]string{"low": "○", "medium": "◐", "high": "●", "xhigh": "◉", "max": "◈"}

// ember is the running marker: one dot whose colour breathes ink → accent → ink every two seconds.
func ember(start time.Time) (string, uv.Style) {
	t := (1 - math.Cos(2*math.Pi*time.Since(start).Seconds()/2)) / 2
	return "●", fg(lerp(th.subtle, th.accent, t))
}

func lerp(a, b color.Color, t float64) color.Color {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	mix := func(x, y uint32) uint8 { return uint8((float64(x>>8)*(1-t) + float64(y>>8)*t) + 0.5) }
	return rgb(mix(ar, br), mix(ag, bg), mix(ab, bb))
}
