// viti-gui — terminal UI for viti, discovered as a plugin by the viti CLI.
// Run "viti gui" (after installing the viti-gui binary on PATH) or invoke
// this binary directly.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/vitistack/vitictl/internal/settings"
)

func main() {
	// termbox puts the terminal in raw mode (ISIG cleared) so Ctrl+C is
	// delivered as <C-c>, but ignoring SIGINT explicitly guards against
	// edge cases where the terminal mode is restored mid-run and the
	// default signal handler would otherwise tear down the TUI before
	// the page binding (e.g. copy on the Secrets detail view) can fire.
	signal.Ignore(syscall.SIGINT)

	if err := settings.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize settings: %v\n", err)
		os.Exit(1)
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "viti-gui: %v\n", err)
		os.Exit(1)
	}
}
