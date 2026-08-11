package pluginmgr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// indexServer serves a fixed plugins.yaml body.
func indexServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// withExtraIndexes overrides the configured extra index sources.
func withExtraIndexes(t *testing.T, urls ...string) {
	t.Helper()
	old := extraIndexes
	extraIndexes = func() []string { return urls }
	t.Cleanup(func() { extraIndexes = old })
}

func names(idx *Index) []string {
	out := make([]string, 0, len(idx.Plugins))
	for _, p := range idx.Plugins {
		out = append(out, p.Name)
	}
	return out
}

func TestFetchIndexMergesConfiguredSources(t *testing.T) {
	clearTokenEnv(t)
	pub := indexServer(t, "plugins:\n  - name: gui\n    repo: vitistack/vitictl\n")
	internal := indexServer(t, "plugins:\n  - name: nhn\n    repo: vitistack/vitictl-nhn\n")
	t.Setenv(IndexEnvVar, pub)
	withExtraIndexes(t, internal)

	idx, err := FetchIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchIndex() error = %v", err)
	}
	got := strings.Join(names(idx), ",")
	if got != "gui,nhn" {
		t.Errorf("merged index = %q, want both sources in order (gui,nhn)", got)
	}
}

// A configured index is more specific than the shared public one, so it wins
// on a name clash — that is what makes overriding an entry possible.
func TestConfiguredIndexOverridesThePublicEntry(t *testing.T) {
	clearTokenEnv(t)
	pub := indexServer(t, "plugins:\n  - name: gui\n    repo: vitistack/vitictl\n    description: public\n")
	internal := indexServer(t, "plugins:\n  - name: gui\n    repo: acme/fork\n    description: internal override\n")
	t.Setenv(IndexEnvVar, pub)
	withExtraIndexes(t, internal)

	idx, err := FetchIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchIndex() error = %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Fatalf("got %d entries, want the duplicate collapsed: %v", len(idx.Plugins), names(idx))
	}
	e, _ := idx.Find("gui")
	if e.Repo != "acme/fork" {
		t.Errorf("repo = %q, want the configured index to win", e.Repo)
	}
	if e.Description != "internal override" {
		t.Errorf("description = %q, want the configured index to win", e.Description)
	}
}

// One unreachable internal index must not break installs of everything else.
func TestFetchIndexToleratesAFailingExtraSource(t *testing.T) {
	clearTokenEnv(t)
	pub := indexServer(t, "plugins:\n  - name: gui\n    repo: vitistack/vitictl\n")
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)
	t.Setenv(IndexEnvVar, pub)
	withExtraIndexes(t, dead.URL)

	var warnings strings.Builder
	oldWarn := warnOut
	warnOut = &warnings
	t.Cleanup(func() { warnOut = oldWarn })

	idx, err := FetchIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchIndex() error = %v, want the reachable source to still work", err)
	}
	if strings.Join(names(idx), ",") != "gui" {
		t.Errorf("index = %v, want just gui", names(idx))
	}
	if !strings.Contains(warnings.String(), dead.URL) {
		t.Errorf("warning = %q, want it to name the index that failed", warnings.String())
	}
}

// If nothing can be read at all, that is an error rather than an empty index.
func TestFetchIndexFailsWhenEverySourceFails(t *testing.T) {
	clearTokenEnv(t)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)
	t.Setenv(IndexEnvVar, dead.URL)
	withExtraIndexes(t)

	if _, err := FetchIndex(context.Background()); err == nil {
		t.Fatal("FetchIndex() = nil error when no source is readable, want one")
	}
}

// Entries flagged private stay out of the default listing, but remain
// installable — the flag controls advertising, not access.
func TestPrivateEntriesAreHiddenFromTheDefaultListing(t *testing.T) {
	clearTokenEnv(t)
	pub := indexServer(t, "plugins:\n"+
		"  - name: gui\n    repo: vitistack/vitictl\n"+
		"  - name: nhn\n    repo: vitistack/vitictl-nhn\n    private: true\n")
	t.Setenv(IndexEnvVar, pub)
	withExtraIndexes(t)

	idx, err := FetchIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchIndex() error = %v", err)
	}

	if got := strings.Join(names(idx.Listable(false)), ","); got != "gui" {
		t.Errorf("Listable(false) = %q, want private entries hidden", got)
	}
	if got := strings.Join(names(idx.Listable(true)), ","); got != "gui,nhn" {
		t.Errorf("Listable(true) = %q, want every entry", got)
	}
	// Hidden, but still resolvable by name so install works.
	if e, ok := idx.Find("nhn"); !ok || !e.Private {
		t.Error("Find(nhn) should still resolve a private entry, flagged as private")
	}
}

// `private` is optional. Omitting it must behave exactly as `private: false`,
// so existing index entries keep working untouched.
func TestPrivateFieldIsOptional(t *testing.T) {
	clearTokenEnv(t)
	src := indexServer(t, "plugins:\n"+
		"  - name: omitted\n    repo: o/a\n"+ // no private key at all
		"  - name: explicit-false\n    repo: o/b\n    private: false\n"+
		"  - name: explicit-true\n    repo: o/c\n    private: true\n")
	t.Setenv(IndexEnvVar, src)
	withExtraIndexes(t)

	idx, err := FetchIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchIndex() error = %v — an entry without `private` must still validate", err)
	}
	if len(idx.Plugins) != 3 {
		t.Fatalf("got %d entries, want 3: %v", len(idx.Plugins), names(idx))
	}

	if e, _ := idx.Find("omitted"); e.Private {
		t.Error("an omitted `private` key should default to false")
	}
	if got := strings.Join(names(idx.Listable(false)), ","); got != "omitted,explicit-false" {
		t.Errorf("Listable(false) = %q, want both non-private entries listed", got)
	}
}

// An entry carrying only the required fields is valid — nothing added for the
// private/index work may become mandatory.
func TestMinimalEntryIsValid(t *testing.T) {
	e := &Entry{Name: "x", Repo: "o/r"}
	if err := e.validate(); err != nil {
		t.Errorf("validate() = %v, want a name and repo to be enough", err)
	}
}
