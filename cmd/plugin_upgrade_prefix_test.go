package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUpgradePrefix(t *testing.T) {
	tmp := t.TempDir()
	recordedDir := filepath.Join(tmp, "old-home", ".local", "bin")
	onPathDir := filepath.Join(tmp, "new-home", ".local", "bin")
	if err := os.MkdirAll(onPathDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// upgradePrefix resolves symlinks, and on macOS the temp dir itself sits
	// behind one (/var -> /private/var).
	onPathDir, err := filepath.EvalSymlinks(onPathDir)
	if err != nil {
		t.Fatal(err)
	}
	onPath := filepath.Join(onPathDir, "viti-gui")
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	found := func(string) (string, error) { return onPath, nil }
	notFound := func(string) (string, error) { return "", errors.New("not on PATH") }

	t.Run("recorded location still exists is kept", func(t *testing.T) {
		prefix, note := upgradePrefix(onPath, notFound, "gui")
		if prefix != onPathDir || note != "" {
			t.Fatalf("got (%q, %q), want (%q, \"\")", prefix, note, onPathDir)
		}
	})

	// The case that matters: a home directory renamed or moved to a new
	// machine. Recreating the recorded folder would put the upgrade off PATH,
	// so the copy viti actually runs is upgraded instead.
	t.Run("missing recorded location upgrades the copy on PATH", func(t *testing.T) {
		prefix, note := upgradePrefix(filepath.Join(recordedDir, "viti-gui"), found, "gui")
		if prefix != onPathDir {
			t.Fatalf("prefix = %q, want %q", prefix, onPathDir)
		}
		if note == "" {
			t.Error("a changed install location should be reported")
		}
		if _, err := os.Stat(recordedDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("recorded folder must not be recreated, stat err = %v", err)
		}
	})

	t.Run("missing recorded location and nothing on PATH uses the default", func(t *testing.T) {
		prefix, note := upgradePrefix(filepath.Join(recordedDir, "viti-gui"), notFound, "gui")
		if prefix != "" {
			t.Fatalf("prefix = %q, want empty so Install picks its default", prefix)
		}
		if note == "" {
			t.Error("a changed install location should be reported")
		}
	})

	t.Run("no recorded path keeps the default silently", func(t *testing.T) {
		prefix, note := upgradePrefix("", found, "gui")
		if prefix != "" || note != "" {
			t.Fatalf("got (%q, %q), want empty", prefix, note)
		}
	})
}
