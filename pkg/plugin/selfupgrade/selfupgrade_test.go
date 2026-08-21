package selfupgrade

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl/pkg/plugin/release"
)

// testOptions is the Options value every test in this file exercises
// against, standing in for a real plugin the way each plugin's own package
// tests stand in for its own binary.
func testOptions() Options {
	return Options{Name: "example", Repo: "vitistack/example", Version: "v1.2.3"}
}

// stubCmd is a bare command wired to in-memory streams, for exercising the
// helpers that read stdin or shell out.
func stubCmd(stdin string) (*cobra.Command, *bytes.Buffer) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
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
			got, err := Confirm(cmd, "Upgrade?")
			if err != nil {
				t.Fatalf("Confirm() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Confirm(%q) = %v, want %v", tt.input, got, tt.want)
			}
			// The prompt must show which way Enter goes.
			if !strings.Contains(out.String(), "[y/N]") {
				t.Errorf("prompt = %q, want it to show the default", out.String())
			}
		})
	}
}

func TestConfirmWithNoInputAtAllIsAnError(t *testing.T) {
	cmd, _ := stubCmd("")
	if _, err := Confirm(cmd, "Upgrade?"); err == nil {
		t.Error("expected an error when stdin closes without an answer")
	}
}

// Regression: /dev/null is a character device, so a file-mode check mistakes
// it for a terminal and the prompt fails with a bare "EOF" instead of saying
// what to do. Anything that is not a terminal must be refused up front.
func TestConfirmRefusesNonTerminalStdin(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = devNull.Close() })

	cmd, _ := stubCmd("")
	cmd.SetIn(devNull)

	_, err = Confirm(cmd, "Upgrade?")
	if err == nil {
		t.Fatal("expected an error when stdin is not a terminal")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error %q should point at --yes", err)
	}
}

// --run execs viti, so a shell without viti has to say what to do rather
// than fail with a bare "executable file not found".
func TestRunPluginUpgradeWithoutVitiOnPathIsActionable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	cmd, _ := stubCmd("")
	err := runPluginUpgrade(cmd, testOptions())
	if err == nil {
		t.Fatal("expected an error when viti is not on PATH")
	}
	for _, want := range []string{"viti", "viti plugin upgrade example"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestUpgradeFlags(t *testing.T) {
	cmd := NewUpgradeCmd(testOptions())
	for _, name := range []string{"run", "yes"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("upgrade is missing the --%s flag", name)
		}
	}
	if f := cmd.Flags().ShorthandLookup("y"); f == nil || f.Name != "yes" {
		t.Error("--yes should be reachable as -y")
	}
}

func TestUpgradeRejectsArguments(t *testing.T) {
	cmd := NewUpgradeCmd(testOptions())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"v1.2.3"})
	if err := cmd.Execute(); err == nil {
		t.Error("expected an error for an unexpected argument")
	}
}

// Both --run and --yes must be discoverable in help.
func TestUpgradeIsRegisteredInParentHelp(t *testing.T) {
	root := &cobra.Command{Use: "viti"}
	root.AddCommand(NewVersionCmd(testOptions()))
	root.AddCommand(NewUpgradeCmd(testOptions()))

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("run(--help) error = %v", err)
	}
	for _, want := range []string{"version", "upgrade"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("--help does not list %q:\n%s", want, out.String())
		}
	}
}

// "viti-example version" must start with the expected prefix.
func TestVersionOutputStartsWithPluginNamePrefix(t *testing.T) {
	cmd := NewVersionCmd(testOptions())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run(version) error = %v", err)
	}
	if !strings.HasPrefix(out.String(), "viti-example version ") {
		t.Errorf("version output = %q, want it to start with %q", out.String(), "viti-example version ")
	}
}

// Without --check the command must not reach the network at all: the test
// suite runs offline, so a hang or an error here means it tried.
func TestVersionWithoutCheckIsPurelyLocal(t *testing.T) {
	cmd := NewVersionCmd(testOptions())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run(version) error = %v", err)
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Errorf("version printed more than the version line:\n%s", out.String())
	}
}

func TestVersionRejectsArguments(t *testing.T) {
	cmd := NewVersionCmd(testOptions())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"extra"})
	if err := cmd.Execute(); err == nil {
		t.Error("expected an error for an unexpected argument")
	}
}

func TestPrintReleaseStatus(t *testing.T) {
	tests := []struct {
		name       string
		local      string
		tag        string
		want       []string
		wantAbsent []string
	}{
		{
			name:  "up to date says so and offers nothing",
			local: "v1.2.3", tag: "v1.2.3",
			want:       []string{"latest release", "v1.2.3"},
			wantAbsent: []string{"viti plugin upgrade example"},
		},
		{
			name:  "outdated names both versions and how to upgrade",
			local: "v1.2.2", tag: "v1.2.3",
			want: []string{"v1.2.3", "v1.2.2", "viti plugin upgrade example", "viti example upgrade --run", "https://example.test"},
		},
		{
			name:  "ahead of the published release is not an upgrade prompt",
			local: "v2.0.0", tag: "v1.2.3",
			want:       []string{"ahead", "v2.0.0", "v1.2.3"},
			wantAbsent: []string{"viti plugin upgrade example"},
		},
		{
			name:  "development build is reported as such",
			local: "dev", tag: "v1.2.3",
			want:       []string{"development build", "dev", "v1.2.3"},
			wantAbsent: []string{"viti plugin upgrade example"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			o := testOptions()
			o.Version = tt.local
			latest := &release.Latest{Tag: tt.tag, URL: "https://example.test/releases/" + tt.tag}
			if err := printReleaseStatus(&buf, o, latest); err != nil {
				t.Fatalf("printReleaseStatus() error = %v", err)
			}
			got := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q:\n%s", want, got)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("output should not contain %q:\n%s", absent, got)
				}
			}
		})
	}
}

// cmdContext is the nil-safety seam: cobra only sets a command's context via
// Execute, so a bare-constructed command handed straight to RunE (as tests
// and embedders do) would otherwise panic in context.WithTimeout /
// exec.CommandContext.
func TestCmdContextFallsBackToBackground(t *testing.T) {
	bare := &cobra.Command{}
	if ctx := cmdContext(bare); ctx == nil {
		t.Fatal("cmdContext(bare command) = nil, want context.Background()")
	}
	want := context.WithValue(context.Background(), ctxKeyForTest{}, "v")
	withCtx := &cobra.Command{}
	withCtx.SetContext(want)
	if got := cmdContext(withCtx); got != want {
		t.Errorf("cmdContext dropped the command's real context")
	}
}

type ctxKeyForTest struct{}

// The private-repo guidance must cover both operations that need the token —
// a user who authenticates only for the check would then watch the upgrade
// fail.
func TestUpgradeHelpSaysBothCheckAndUpgradeNeedTheToken(t *testing.T) {
	cmd := NewUpgradeCmd(Options{Name: "example", Repo: "vitistack/example", Version: "v1.0.0"})
	unwrapped := strings.ReplaceAll(cmd.Long, "\n", " ")
	if !strings.Contains(unwrapped, "the check and the upgrade need a GitHub token") {
		t.Errorf("upgrade Long %q should say the check AND the upgrade need the token", cmd.Long)
	}
}
