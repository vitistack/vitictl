package cmd

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// stubCmd is a bare command wired to in-memory streams, for exercising the
// prompt without a terminal.
func stubCmd(stdin string) (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	return cmd, &out
}

func TestConfirm(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"y", "y\n", true},
		{"yes spelled out, any case", "YES\n", true},
		{"n", "n\n", false},
		{"a bare newline takes the default", "\n", false},
		{"an answer without a trailing newline still counts", "y", true},
		{"anything unrecognised is a no", "maybe\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, out := stubCmd(tt.input)
			got, err := confirm(cmd, "Upgrade?")
			if err != nil {
				t.Fatalf("confirm() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("confirm(%q) = %v, want %v", tt.input, got, tt.want)
			}
			// The prompt must show which way Enter goes.
			if !strings.Contains(out.String(), "[y/N]") {
				t.Errorf("prompt = %q, want it to show the default", out.String())
			}
		})
	}
}

// Regression: /dev/null is a character device, so the file-mode check this
// replaced mistook it for a terminal — `viti upgrade --run < /dev/null` got
// past the guard and then died on the read with a bare "EOF". Anything that
// is not a terminal must be refused up front, with advice.
func TestConfirmRefusesNonTerminalStdin(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = devNull.Close() })

	cmd, _ := stubCmd("")
	cmd.SetIn(devNull)

	_, err = confirm(cmd, "Upgrade?")
	if err == nil {
		t.Fatal("expected an error when stdin is not a terminal")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error %q should point at --yes", err)
	}
}

func TestConfirmWithNoInputAtAllIsAnError(t *testing.T) {
	cmd, _ := stubCmd("")
	if _, err := confirm(cmd, "Upgrade?"); err == nil {
		t.Error("expected an error when stdin closes without an answer")
	}
}
