package tui

import (
	uv "github.com/charmbracelet/ultraviolet"
)

type keyCode int

const (
	keyNone keyCode = iota
	keyEnter
	keyNewline // Shift+Enter, Alt+Enter or Ctrl-J
	keyEsc
	keyBackspace
	keyCtrlB
	keyCtrlC
	keyCtrlD
	keyCtrlF
	keyCtrlU
	keyCtrlW
	keyPageUp
	keyPageDown
	keyUp
	keyDown
	keyLeft
	keyRight
)

var specialKeys = map[string]keyCode{
	"enter":       keyEnter,
	"shift+enter": keyNewline,
	"alt+enter":   keyNewline,
	"ctrl+j":      keyNewline,
	"esc":         keyEsc,
	"backspace":   keyBackspace,
	"ctrl+h":      keyBackspace,
	"ctrl+b":      keyCtrlB,
	"ctrl+c":      keyCtrlC,
	"ctrl+d":      keyCtrlD,
	"ctrl+f":      keyCtrlF,
	"ctrl+u":      keyCtrlU,
	"ctrl+w":      keyCtrlW,
	"pgup":        keyPageUp,
	"pgdown":      keyPageDown,
	"up":          keyUp,
	"down":        keyDown,
	"left":        keyLeft,
	"right":       keyRight,
}

func translateKey(msg uv.KeyPressEvent) (rune, keyCode) {
	if key, ok := specialKeys[msg.String()]; ok {
		return 0, key
	}
	if msg.Mod&(uv.ModCtrl|uv.ModAlt|uv.ModMeta|uv.ModSuper) != 0 {
		return 0, keyNone
	}
	chars := []rune(msg.Text)
	if len(chars) != 1 || chars[0] < 0x20 || chars[0] == 0x7f {
		return 0, keyNone
	}
	return chars[0], keyNone
}
