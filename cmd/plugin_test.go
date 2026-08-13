package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/vitistack/vitictl/internal/plugin"
	"github.com/vitistack/vitictl/internal/pluginmgr"
)

// readShippedIndex parses the plugins.yaml this repo publishes.
func readShippedIndex(t *testing.T) *pluginmgr.Index {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "plugins.yaml"))
	if err != nil {
		t.Fatalf("reading plugins.yaml: %v", err)
	}
	var idx pluginmgr.Index
	if err := yaml.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("parsing plugins.yaml: %v", err)
	}
	return &idx
}

// The whole point of declaring aliases centrally: a clash is caught here, in
// the pull request that adds one, rather than silently on every machine that
// installs the plugin. viti's own commands win dispatch, so an alias that
// collides with one would never fire and never explain itself.
func TestShippedAliasesDoNotCollideWithBuiltins(t *testing.T) {
	builtins := builtinCommandNames()
	for _, e := range readShippedIndex(t).Plugins {
		for _, a := range e.Aliases {
			if builtins[a] {
				t.Errorf("plugin %q declares alias %q, which is already a viti command "+
					"— `viti %s` would never reach the plugin", e.Name, a, a)
			}
		}
	}
}

// Two plugins claiming the same alias is decided by PATH order, which is not
// a decision anyone made.
func TestShippedAliasesAreUniqueAcrossPlugins(t *testing.T) {
	owner := map[string]string{}
	for _, e := range readShippedIndex(t).Plugins {
		for _, a := range e.Aliases {
			if prior, dup := owner[a]; dup {
				t.Errorf("alias %q is claimed by both %q and %q", a, prior, e.Name)
				continue
			}
			owner[a] = e.Name
		}
		if _, clash := owner[e.Name]; clash && owner[e.Name] != e.Name {
			t.Errorf("plugin %q shares its name with an alias of %q", e.Name, owner[e.Name])
		}
	}
}

// Every entry must survive the validation the installer applies at fetch time,
// so a malformed alias never reaches a user.
func TestShippedIndexEntriesAreValid(t *testing.T) {
	idx := readShippedIndex(t)
	for i := range idx.Plugins {
		if err := idx.Plugins[i].Validate(); err != nil {
			t.Errorf("plugins.yaml entry %q: %v", idx.Plugins[i].Name, err)
		}
	}
}

// An alias is the same install under another name; showing it as its own row
// implies two plugins are installed.
func TestAliasOwnersFoldsRecordedAliases(t *testing.T) {
	found := []plugin.Plugin{
		{Name: "kubevirt", Path: "/bin/viti-kubevirt"},
		{Name: "kv", Path: "/bin/viti-kv"},
		{Name: "nhn", Path: "/bin/viti-nhn"},
	}
	managed := map[string]*pluginmgr.State{
		"kubevirt": {Name: "kubevirt", Aliases: []string{"kv"}},
	}

	got := aliasOwners(found, managed)
	if got["kv"] != "kubevirt" {
		t.Errorf("aliasOwners()[kv] = %q, want kubevirt", got["kv"])
	}
	if _, folded := got["nhn"]; folded {
		t.Error("nhn is its own plugin and must keep its row")
	}
	if _, folded := got["kubevirt"]; folded {
		t.Error("the owning plugin must keep its row")
	}
}

// A link made by hand — or by a viti that predates alias tracking — is still
// an alias, and is folded in by resolving to the same file.
func TestAliasOwnersFoldsAnUntrackedSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "viti-kubevirt")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil { // #nosec G306 -- test binary
		t.Fatal(err)
	}
	link := filepath.Join(dir, "viti-kv")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got := aliasOwners([]plugin.Plugin{
		{Name: "kubevirt", Path: real},
		{Name: "kv", Path: link},
	}, nil)
	if got["kv"] != "kubevirt" {
		t.Errorf("an untracked symlink was not folded: %v", got)
	}
}
