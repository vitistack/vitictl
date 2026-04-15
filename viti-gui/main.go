// viti-gui — terminal UI for viti, discovered as a plugin by the viti CLI.
// Run "viti gui" (after installing the viti-gui binary on PATH) or invoke
// this binary directly.
package main

import (
	"fmt"
	"os"

	"github.com/vitistack/vitictl/internal/settings"
)

func main() {
	if err := settings.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize settings: %v\n", err)
		os.Exit(1)
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "viti-gui: %v\n", err)
		os.Exit(1)
	}
}
