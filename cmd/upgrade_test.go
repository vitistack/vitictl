package cmd

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl/internal/release"
)

// planRun decides what `viti upgrade --run` executes: the installer for viti
// itself, the all-plugins pass, or both. The self and plugin halves are
// independent — an up-to-date viti must still upgrade outdated plugins, and
// Windows (where the installer cannot replace the running exe) must still get
// its plugins upgraded rather than an outright refusal.
func TestPlanRun(t *testing.T) {
	tests := []struct {
		name      string
		status    release.Status
		goos      string
		noPlugins bool
		plugins   int
		want      runPlan
	}{
		{"outdated with plugins", release.StatusOutdated, "linux", false, 2,
			runPlan{installer: true, plugins: true}},
		{"up-to-date viti still upgrades plugins", release.StatusUpToDate, "linux", false, 2,
			runPlan{plugins: true}},
		{"outdated, --no-plugins", release.StatusOutdated, "linux", true, 2,
			runPlan{installer: true}},
		{"nothing installed, nothing outdated", release.StatusUpToDate, "linux", false, 0,
			runPlan{}},
		{"development build uses the installer", release.StatusDevelopment, "linux", false, 0,
			runPlan{installer: true}},
		{"windows: no installer, but plugins proceed", release.StatusOutdated, "windows", false, 1,
			runPlan{windowsHint: true, plugins: true}},
		{"ahead of the release: no installer", release.StatusAhead, "linux", false, 1,
			runPlan{plugins: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planRun(tt.status, tt.goos, tt.noPlugins, tt.plugins)
			if got != tt.want {
				t.Errorf("planRun = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// One line per plugin in check mode, and a check that fails (private repo
// without a token, offline) is reported without failing the command.
func TestPluginStatusLine(t *testing.T) {
	line := pluginStatusLine("nhn", "v0.1.0", "v0.1.1", nil)
	for _, want := range []string{"nhn", "v0.1.0", "v0.1.1"} {
		if !strings.Contains(line, want) {
			t.Errorf("outdated line %q missing %q", line, want)
		}
	}
	line = pluginStatusLine("kubevirt", "v0.1.1", "v0.1.1", nil)
	if !strings.Contains(line, "up to date") {
		t.Errorf("current line %q should say up to date", line)
	}
	line = pluginStatusLine("nhn", "v0.1.0", "", errors.New("github API returned 404"))
	if !strings.Contains(line, "404") || !strings.Contains(line, "nhn") {
		t.Errorf("failed check %q should name the plugin and the reason", line)
	}
}

// The one confirmation covers exactly what will run.
func TestBundledPrompt(t *testing.T) {
	got := bundledPrompt("v0.3.0", true, 2)
	for _, want := range []string{"v0.3.0", "2"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt %q missing %q", got, want)
		}
	}
	got = bundledPrompt("v0.3.0", false, 2)
	if strings.Contains(got, "v0.3.0") {
		t.Errorf("plugins-only prompt %q must not promise a viti upgrade", got)
	}
	got = bundledPrompt("v0.3.0", true, 0)
	if !strings.Contains(got, "v0.3.0") || strings.Contains(got, "plugin") {
		t.Errorf("self-only prompt %q must not mention plugins", got)
	}
}

func TestUpgradeHasNoPluginsFlag(t *testing.T) {
	if upgradeCmd.Flags().Lookup("no-plugins") == nil {
		t.Fatal("upgrade must offer --no-plugins to keep the self-only behavior")
	}
}

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
