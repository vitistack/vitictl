package pluginmgr

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vitistack/vitictl/internal/settings"
)

// stateDirName lives under settings.ConfigDir() so user-scoped plugin
// metadata sits next to ctl.config.yaml.
const stateDirName = "plugins"

// State records everything vitictl needs to upgrade or uninstall a
// plugin without re-reading the index.
type State struct {
	Name        string    `json:"name"`
	Repo        string    `json:"repo"`
	Version     string    `json:"version"`
	BinaryPath  string    `json:"binaryPath"`
	SHA256      string    `json:"sha256,omitempty"`
	InstalledAt time.Time `json:"installedAt"`
}

// StateDir returns ~/.vitistack/plugins (created on demand).
func StateDir() (string, error) {
	base, err := settings.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, stateDirName), nil
}

func ensureStateDir() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating plugin state dir: %w", err)
	}
	return dir, nil
}

func statePath(name string) (string, error) {
	dir, err := ensureStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// ReadState returns the saved state for a plugin, or (nil, nil) if no
// state file exists.
func ReadState(name string) (*State, error) {
	path, err := statePath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path is derived from validated plugin name under ~/.vitistack/plugins.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing state for %q: %w", name, err)
	}
	return &s, nil
}

// WriteState persists a plugin's state to disk.
func WriteState(s *State) error {
	if s.Name == "" {
		return errors.New("state missing plugin name")
	}
	path, err := statePath(s.Name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// DeleteState removes a plugin's state file. A missing file is not an
// error.
func DeleteState(name string) error {
	path, err := statePath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ListStates returns all saved plugin states, sorted by name.
func ListStates() ([]*State, error) {
	dir, err := StateDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*State
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		s, err := ReadState(name)
		if err != nil || s == nil {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
