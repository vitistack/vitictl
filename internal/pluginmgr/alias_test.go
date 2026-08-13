package pluginmgr

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A built-in wins dispatch outright, so an alias colliding with one would
// silently never fire. That is the case worth refusing loudest.
func TestCheckAliasesRefusesABuiltinName(t *testing.T) {
	entry := &Entry{Name: "kubevirt", Repo: "vitistack/vitictl-kubevirt", Aliases: []string{"kv", "kc"}}
	got := CheckAliases(entry, map[string]bool{"kc": true, "machine": true}, nil)

	if len(got) != 1 {
		t.Fatalf("got %d conflicts, want 1: %v", len(got), got)
	}
	if got[0].Alias != "kc" {
		t.Errorf("conflicting alias = %q, want kc", got[0].Alias)
	}
	if !strings.Contains(got[0].Reason, "built-in") {
		t.Errorf("reason %q should say it is a built-in", got[0].Reason)
	}
}

func TestCheckAliasesRefusesAnotherPluginsName(t *testing.T) {
	entry := &Entry{Name: "kubevirt", Aliases: []string{"nhn"}}
	got := CheckAliases(entry, nil, map[string]string{"nhn": "/usr/local/bin/viti-nhn"})

	if len(got) != 1 {
		t.Fatalf("got %d conflicts, want 1", len(got))
	}
	if !strings.Contains(got[0].Reason, "viti-nhn") {
		t.Errorf("reason %q should name the binary already there", got[0].Reason)
	}
}

func TestCheckAliasesAcceptsAFreeName(t *testing.T) {
	entry := &Entry{Name: "kubevirt", Aliases: []string{"kv"}}
	if got := CheckAliases(entry, map[string]bool{"kvc": true}, map[string]string{"nhn": "/x"}); len(got) != 0 {
		t.Errorf("kv should be free alongside kvc and nhn, got %v", got)
	}
}

func TestJoinConflictsIsActionable(t *testing.T) {
	err := JoinConflicts("kubevirt", []Conflict{{Alias: "kc", Reason: "taken"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"kubevirt", "kc", "--no-aliases"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	if JoinConflicts("kubevirt", nil) != nil {
		t.Error("no conflicts should produce no error")
	}
}

// The link is relative so that moving the install directory does not strand
// it — an absolute target would break the moment ~/.local/bin was relocated.
func TestLinkAliasIsRelativeAndResolves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows; the copy fallback is exercised by hand")
	}
	dir := t.TempDir()
	binary := "viti-kubevirt"
	if err := os.WriteFile(filepath.Join(dir, binary), []byte("#!/bin/sh\n"), 0o755); err != nil { // #nosec G306 -- test binary must be executable
		t.Fatal(err)
	}

	if err := linkAlias(dir, binary, "kv"); err != nil {
		t.Fatalf("linkAlias() error = %v", err)
	}
	link := filepath.Join(dir, "viti-kv")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("Readlink() error = %v", err)
	}
	if target != binary {
		t.Errorf("link target = %q, want the bare filename %q", target, binary)
	}
	if _, err := os.Stat(link); err != nil {
		t.Errorf("link does not resolve: %v", err)
	}
}

// Re-linking is what an upgrade does every time; it must not fail on the
// second run.
func TestLinkAliasReplacesAnExistingLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "viti-kubevirt"), []byte("x"), 0o755); err != nil { // #nosec G306 -- test binary
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := linkAlias(dir, "viti-kubevirt", "kv"); err != nil {
			t.Fatalf("linkAlias() run %d error = %v", i+1, err)
		}
	}
}

func TestUnlinkAliasRemovesOurOwnLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "viti-kubevirt"), []byte("x"), 0o755); err != nil { // #nosec G306 -- test binary
		t.Fatal(err)
	}
	if err := linkAlias(dir, "viti-kubevirt", "kv"); err != nil {
		t.Fatal(err)
	}
	if err := unlinkAlias(dir, "viti-kubevirt", "kv"); err != nil {
		t.Fatalf("unlinkAlias() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "viti-kv")); !os.IsNotExist(err) {
		t.Error("the alias link survived uninstall")
	}
}

// Uninstalling a plugin must not delete a stranger's binary that happens to
// share the alias name.
func TestUnlinkAliasLeavesSomeoneElsesBinary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "viti-kubevirt"), []byte("ours"), 0o755); err != nil { // #nosec G306 -- test binary
		t.Fatal(err)
	}
	stranger := filepath.Join(dir, "viti-kv")
	if err := os.WriteFile(stranger, []byte("someone else entirely"), 0o755); err != nil { // #nosec G306 -- test binary
		t.Fatal(err)
	}

	if err := unlinkAlias(dir, "viti-kubevirt", "kv"); err != nil {
		t.Fatalf("unlinkAlias() error = %v", err)
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Errorf("an unrelated viti-kv was deleted: %v", err)
	}
}

// A copy is what the Windows fallback leaves behind; uninstall must still
// recognise it as ours.
func TestUnlinkAliasRemovesAnIdenticalCopy(t *testing.T) {
	dir := t.TempDir()
	body := []byte("the same bytes")
	if err := os.WriteFile(filepath.Join(dir, "viti-kubevirt"), body, 0o755); err != nil { // #nosec G306 -- test binary
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "viti-kv"), body, 0o755); err != nil { // #nosec G306 -- test binary
		t.Fatal(err)
	}

	if err := unlinkAlias(dir, "viti-kubevirt", "kv"); err != nil {
		t.Fatalf("unlinkAlias() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "viti-kv")); !os.IsNotExist(err) {
		t.Error("the copied alias survived uninstall")
	}
}

// The case that makes reconciliation necessary: a plugin installed before the
// index declared an alias never reinstalls once it is on the latest release,
// so upgrading has to create the link or the alias reaches new users only.
func TestEnsureAliasesCreatesWhatIsMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "viti-kubevirt")
	if err := os.WriteFile(binary, []byte("x"), 0o755); err != nil { // #nosec G306 -- test binary
		t.Fatal(err)
	}
	state := &State{Name: "kubevirt", BinaryPath: binary}

	have, err := EnsureAliases(state, []string{"kv"}, nil)
	if err != nil {
		t.Fatalf("EnsureAliases() error = %v", err)
	}
	if len(have) != 1 || have[0] != "kv" {
		t.Fatalf("EnsureAliases() = %v, want [kv]", have)
	}
	if _, err := os.Stat(filepath.Join(dir, "viti-kv")); err != nil {
		t.Errorf("the alias was not created: %v", err)
	}

	// Running again must be a no-op, not a churn: upgrade calls this every
	// time, including when nothing changed.
	again, err := EnsureAliases(state, []string{"kv"}, nil)
	if err != nil {
		t.Fatalf("second EnsureAliases() error = %v", err)
	}
	if len(again) != 1 {
		t.Errorf("second run reported %v, want the same single alias", again)
	}
}

// A name already taken by something else is left alone and not claimed.
func TestEnsureAliasesLeavesAStrangersBinary(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "viti-kubevirt")
	if err := os.WriteFile(binary, []byte("ours"), 0o755); err != nil { // #nosec G306 -- test binary
		t.Fatal(err)
	}
	stranger := filepath.Join(dir, "viti-kv")
	if err := os.WriteFile(stranger, []byte("theirs"), 0o755); err != nil { // #nosec G306 -- test binary
		t.Fatal(err)
	}

	have, err := EnsureAliases(&State{Name: "kubevirt", BinaryPath: binary}, []string{"kv"}, nil)
	if err != nil {
		t.Fatalf("EnsureAliases() error = %v", err)
	}
	if len(have) != 0 {
		t.Errorf("EnsureAliases() claimed %v, want nothing", have)
	}
	body, err := os.ReadFile(stranger) // #nosec G304 -- test fixture
	if err != nil || string(body) != "theirs" {
		t.Error("the unrelated binary was overwritten")
	}
}

func TestEnsureAliasesNeedsAnInstalledBinary(t *testing.T) {
	if _, err := EnsureAliases(nil, []string{"kv"}, nil); err == nil {
		t.Error("expected an error for a nil state")
	}
	if _, err := EnsureAliases(&State{Name: "x"}, []string{"kv"}, nil); err == nil {
		t.Error("expected an error when the binary path is unknown")
	}
}

func TestUnlinkAliasToleratesAMissingLink(t *testing.T) {
	dir := t.TempDir()
	if err := unlinkAlias(dir, "viti-kubevirt", "kv"); err != nil {
		t.Errorf("unlinkAlias() on nothing = %v, want no error", err)
	}
}

// An alias becomes a filename on PATH, so anything a path or shell would
// reinterpret has to be refused when the index is read.
func TestEntryValidateRejectsUnusableAliases(t *testing.T) {
	for name, alias := range map[string]string{
		"empty":          "",
		"path separator": "a/b",
		"dotted":         "a.b",
		"spaced":         "a b",
		"repeats name":   "kubevirt",
	} {
		t.Run(name, func(t *testing.T) {
			e := Entry{Name: "kubevirt", Repo: "vitistack/vitictl-kubevirt", Aliases: []string{alias}}
			if err := e.validate(); err == nil {
				t.Errorf("alias %q was accepted", alias)
			}
		})
	}

	dup := Entry{Name: "kubevirt", Repo: "a/b", Aliases: []string{"kv", "kv"}}
	if err := dup.validate(); err == nil {
		t.Error("a repeated alias was accepted")
	}

	ok := Entry{Name: "kubevirt", Repo: "a/b", Aliases: []string{"kv"}}
	if err := ok.validate(); err != nil {
		t.Errorf("validate() = %v, want nil for a sound entry", err)
	}
}
