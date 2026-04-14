package main

import (
	"fmt"
	"os"

	"github.com/vitistack/vitictl/cmd"
	"github.com/vitistack/vitictl/internal/settings"
)

func main() {
	if err := settings.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize settings: %v\n", err)
		os.Exit(1)
	}
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
