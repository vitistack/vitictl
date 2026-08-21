package viticli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPathReportsAMissingViti(t *testing.T) {
	orig := Binary
	Binary = "definitely-not-a-real-binary-anywhere"
	defer func() { Binary = orig }()

	_, err := Path()
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Path() error = %v, want ErrNotInstalled", err)
	}
}

// Run executes the resolved binary with the given args and wired streams.
func TestRunExecutesTheBinary(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "viti-stub")
	script := "#!/bin/sh\necho \"$@\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := Binary
	Binary = stub
	defer func() { Binary = orig }()

	var out strings.Builder
	err := Run(context.Background(),
		Streams{In: strings.NewReader(""), Out: &out, Err: &out},
		[]string{"kubevirt", "vm", "changemachineclass", "web-1", "--class", "large"},
		PluginDiagnosis([]string{"kubevirt", "vm", "changemachineclass"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "kubevirt vm changemachineclass web-1 --class large") {
		t.Errorf("stub saw %q", out.String())
	}
}

// An installed viti whose kubevirt plugin predates changemachineclass fails
// with cobra's raw "unknown command" — the error must point at upgrading the
// plugin instead. The stub answers --help for viti itself and for "kubevirt",
// but not for the changemachineclass subcommand: a plugin that exists but is
// too old.
func TestRunHintsAtUpgradeWhenTheSubcommandIsMissing(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "viti-stub")
	script := `#!/bin/sh
if [ "$1" = --help ]; then exit 0; fi
if [ "$1" = kubevirt ] && [ "$2" = --help ]; then exit 0; fi
echo 'Error: unknown command "changemachineclass"' >&2
exit 1
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := Binary
	Binary = stub
	defer func() { Binary = orig }()

	var out strings.Builder
	err := Run(context.Background(),
		Streams{In: strings.NewReader(""), Out: &out, Err: &out},
		[]string{"kubevirt", "vm", "changemachineclass", "web-1", "--yes"},
		PluginDiagnosis([]string{"kubevirt", "vm", "changemachineclass"}))
	if err == nil {
		t.Fatal("expected an error from the failing stub")
	}
	if !strings.Contains(err.Error(), "upgrade") {
		t.Errorf("error does not hint at upgrading the kubevirt plugin: %v", err)
	}
}

// A viti that knows the subcommand but fails for a real reason (bad flag
// value, unreachable cluster) must not be blamed on an old plugin.
func TestRunDoesNotHintAtUpgradeForOrdinaryFailures(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "viti-stub")
	// The probe (last arg --help) succeeds; the real invocation fails.
	script := "#!/bin/sh\nfor a; do last=$a; done\nif [ \"$last\" = --help ]; then exit 0; fi\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := Binary
	Binary = stub
	defer func() { Binary = orig }()

	var out strings.Builder
	err := Run(context.Background(),
		Streams{In: strings.NewReader(""), Out: &out, Err: &out},
		[]string{"kubevirt", "vm", "changemachineclass", "web-1", "--yes"},
		PluginDiagnosis([]string{"kubevirt", "vm", "changemachineclass"}))
	if err == nil {
		t.Fatal("expected an error from the failing stub")
	}
	if strings.Contains(err.Error(), "upgrade") {
		t.Errorf("ordinary failure misdiagnosed as an old plugin: %v", err)
	}
}

// An ordinary child failure already printed its own "❌ Error:" line; the
// wrapper must not print a second one. Run marks it with ErrChildFailed so
// the root command exits non-zero silently.
func TestRunOrdinaryFailureIsMarkedChildFailed(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "viti-stub")
	script := "#!/bin/sh\nfor a; do last=$a; done\nif [ \"$last\" = --help ]; then exit 0; fi\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := Binary
	Binary = stub
	defer func() { Binary = orig }()

	err := Run(context.Background(),
		Streams{In: strings.NewReader(""), Out: &strings.Builder{}, Err: &strings.Builder{}},
		[]string{"kubevirt", "vm", "changemachineclass", "web-1", "--yes"},
		PluginDiagnosis([]string{"kubevirt", "vm", "changemachineclass"}))
	if !errors.Is(err, ErrChildFailed) {
		t.Fatalf("err = %v, want ErrChildFailed", err)
	}
}

// A caller with no diagnosis (shelling out to a built-in viti command, like
// kubevirt's Talos-dashboard delegation) gets the plain classification:
// normal non-zero exits are the child's own report.
func TestRunWithoutDiagnosisMarksNormalFailuresChildFailed(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "viti-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := Binary
	Binary = stub
	defer func() { Binary = orig }()

	err := Run(context.Background(),
		Streams{In: strings.NewReader(""), Out: &strings.Builder{}, Err: &strings.Builder{}},
		[]string{"machine", "console", "web-1"}, nil)
	if !errors.Is(err, ErrChildFailed) {
		t.Fatalf("err = %v, want ErrChildFailed", err)
	}
}

// The upgrade hint is the wrapper's own message — it must NOT be silent.
func TestRunUpgradeHintIsNotMarkedChildFailed(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "viti-stub")
	script := `#!/bin/sh
if [ "$1" = --help ]; then exit 0; fi
if [ "$1" = kubevirt ] && [ "$2" = --help ]; then exit 0; fi
exit 1
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := Binary
	Binary = stub
	defer func() { Binary = orig }()

	err := Run(context.Background(),
		Streams{In: strings.NewReader(""), Out: &strings.Builder{}, Err: &strings.Builder{}},
		[]string{"kubevirt", "vm", "changemachineclass", "web-1", "--yes"},
		PluginDiagnosis([]string{"kubevirt", "vm", "changemachineclass"}))
	if err == nil || errors.Is(err, ErrChildFailed) {
		t.Fatalf("err = %v, want a loud upgrade-hint error", err)
	}
}

// A viti without the kubevirt plugin at all fails the same way as an old one
// ("unknown command"), but the fix is different: prescribing an upgrade of a
// plugin that was never installed sends the user to a command that cannot
// help. viti itself answering --help while "viti kubevirt --help" fails is
// what tells the two apart.
func TestRunHintsAtInstallWhenThePluginIsMissing(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "viti-stub")
	script := `#!/bin/sh
if [ "$1" = --help ]; then exit 0; fi
echo 'Error: unknown command "kubevirt" for "viti"' >&2
exit 1
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := Binary
	Binary = stub
	defer func() { Binary = orig }()

	err := Run(context.Background(),
		Streams{In: strings.NewReader(""), Out: &strings.Builder{}, Err: &strings.Builder{}},
		[]string{"kubevirt", "vm", "changemachineclass", "web-1", "--yes"},
		PluginDiagnosis([]string{"kubevirt", "vm", "changemachineclass"}))
	if err == nil {
		t.Fatal("expected an error from the failing stub")
	}
	if !strings.Contains(err.Error(), "viti plugin install kubevirt") {
		t.Errorf("error does not point at installing the plugin: %v", err)
	}
	if strings.Contains(err.Error(), "upgrade") {
		t.Errorf("missing plugin misdiagnosed as an old one: %v", err)
	}
}

// Cancelling mid-run (Ctrl+C in the child's picker or prompt) kills the child
// through the context. Probing afterwards runs on the same cancelled context
// and fails instantly, so a plain user cancel must not be misread as an
// outdated plugin — and it needs no loud report either.
func TestRunCancelledContextIsNotMisdiagnosed(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "viti-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := Binary
	Binary = stub
	defer func() { Binary = orig }()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := Run(ctx,
		Streams{In: strings.NewReader(""), Out: &strings.Builder{}, Err: &strings.Builder{}},
		[]string{"kubevirt", "vm", "changemachineclass", "web-1", "--yes"},
		PluginDiagnosis([]string{"kubevirt", "vm", "changemachineclass"}))
	if err == nil {
		t.Fatal("expected an error from the cancelled run")
	}
	if strings.Contains(err.Error(), "upgrade") || strings.Contains(err.Error(), "install") {
		t.Errorf("a user cancel was misdiagnosed as a plugin problem: %v", err)
	}
	if !errors.Is(err, ErrChildFailed) {
		t.Errorf("err = %v, want the silent ErrChildFailed marker for a cancel", err)
	}
}

// PluginDiagnosis(nil) must not panic on an empty probe slice — it has
// nothing to diagnose, so it falls back to the plain ErrChildFailed marker.
func TestPluginDiagnosisWithNilProbeDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "viti-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := Binary
	Binary = stub
	defer func() { Binary = orig }()

	err := Run(context.Background(),
		Streams{In: strings.NewReader(""), Out: &strings.Builder{}, Err: &strings.Builder{}},
		[]string{"kubevirt", "vm", "changemachineclass", "web-1", "--yes"},
		PluginDiagnosis(nil))
	if !errors.Is(err, ErrChildFailed) {
		t.Fatalf("err = %v, want ErrChildFailed", err)
	}
}

// A child killed before it could write anything (OOM kill, SIGTERM from
// session cleanup) exits non-zero having reported nothing. Marking that
// ErrChildFailed would make the wrapper exit non-zero in total silence — the
// one case the "child already reported" assumption does not hold.
func TestRunChildKilledWithoutReportingIsLoud(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "viti-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nkill -KILL $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := Binary
	Binary = stub
	defer func() { Binary = orig }()

	err := Run(context.Background(),
		Streams{In: strings.NewReader(""), Out: &strings.Builder{}, Err: &strings.Builder{}},
		[]string{"kubevirt", "vm", "changemachineclass", "web-1", "--yes"},
		PluginDiagnosis([]string{"kubevirt", "vm", "changemachineclass"}))
	if err == nil {
		t.Fatal("expected an error from the killed stub")
	}
	if errors.Is(err, ErrChildFailed) {
		t.Errorf("err = %v marked silent, but the child never reported anything", err)
	}
	if strings.Contains(err.Error(), "upgrade") || strings.Contains(err.Error(), "install") {
		t.Errorf("a killed child was misdiagnosed as a plugin problem: %v", err)
	}
}
