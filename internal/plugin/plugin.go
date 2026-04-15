// Package plugin implements kubectl-style external subcommand discovery for
// viti. Any executable on PATH whose basename begins with the Prefix is
// exposed as a subcommand — e.g. a binary named "viti-foo" can be invoked
// as "viti foo [args...]".
package plugin

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Prefix is the required binary-name prefix for viti plugins.
const Prefix = "viti-"

// Plugin describes a discovered plugin binary on PATH.
type Plugin struct {
	// Name is the plugin identifier (binary basename with Prefix stripped
	// and, on Windows, the .exe suffix removed).
	Name string
	// Path is the absolute path to the plugin binary.
	Path string
}

// Find returns the resolved path to viti-<name> on PATH.
func Find(name string) (string, error) {
	bin := Prefix + name
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	return exec.LookPath(bin)
}

// LongestMatch walks tokens left-to-right and tries progressively shorter
// joined names until a matching plugin is found on PATH. For tokens
// ["foo", "bar", "baz"] it tries viti-foo-bar-baz, then viti-foo-bar,
// then viti-foo.
func LongestMatch(tokens []string) (path string, matched []string, ok bool) {
	for i := len(tokens); i > 0; i-- {
		name := strings.Join(tokens[:i], "-")
		if name == "" {
			continue
		}
		if p, err := Find(name); err == nil {
			return p, tokens[:i], true
		}
	}
	return "", nil, false
}

// List discovers every viti-* binary on PATH. Results are sorted by name;
// callers can detect shadowing (same name appearing multiple times) by
// walking the slice in order.
func List() ([]Plugin, error) {
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		return nil, nil
	}
	seenPath := make(map[string]struct{})
	var out []Plugin
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if !strings.HasPrefix(n, Prefix) || e.IsDir() {
				continue
			}
			full := filepath.Join(dir, n)
			if _, dup := seenPath[full]; dup {
				continue
			}
			seenPath[full] = struct{}{}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
				continue
			}
			name := strings.TrimPrefix(n, Prefix)
			if runtime.GOOS == "windows" {
				name = strings.TrimSuffix(name, ".exe")
			}
			if name == "" {
				continue
			}
			out = append(out, Plugin{Name: name, Path: full})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

// Exec replaces (on Unix) or wraps (on Windows) the current process with
// the plugin binary. argv is the plain argument list — the path is
// prepended automatically to form argv[0].
func Exec(path string, argv, env []string) error {
	full := make([]string, 0, len(argv)+1)
	full = append(full, path)
	full = append(full, argv...)
	return execPlugin(path, full, env)
}
