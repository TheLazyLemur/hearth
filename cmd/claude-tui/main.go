package main

import (
	"fmt"
	"os"

	"claude-code-tui/cmd/claude-tui/app"
)

func main() {
	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
