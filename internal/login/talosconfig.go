package login

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"sigs.k8s.io/yaml"
)

// TalosconfigOutcome reports where a talosconfig was written and under
// which context, plus the endpoints that ended up in that context.
type TalosconfigOutcome struct {
	Path      string
	Context   string
	Endpoints []string
	Activated bool
	Overwrote bool
}

// talosConfig mirrors the subset of the talosconfig YAML structure we
// need to merge. Unknown fields on both the source and target side are
// preserved verbatim via map[string]any.
type talosConfig struct {
	Context  string                    `json:"context,omitempty"`
	Contexts map[string]*talosCtxEntry `json:"contexts,omitempty"`
}

type talosCtxEntry struct {
	Endpoints []string       `json:"endpoints,omitempty"`
	Nodes     []string       `json:"nodes,omitempty"`
	CA        string         `json:"ca,omitempty"`
	Crt       string         `json:"crt,omitempty"`
	Key       string         `json:"key,omitempty"`
	Extra     map[string]any `json:"-"`
}

// MergeTalosconfig merges the incoming talosconfig bytes into the
// talosconfig at `targetPath` under `contextName`. If `endpoints` is
// non-empty, it replaces whatever endpoints were in the incoming
// talosconfig. If `targetPath` is empty, the user's default talosconfig
// is resolved ($TALOSCONFIG → ~/.talos/config).
func MergeTalosconfig(data []byte, contextName string, endpoints []string, targetPath string, activate, force bool) (*TalosconfigOutcome, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty talosconfig")
	}
	if contextName == "" {
		return nil, fmt.Errorf("context name is required")
	}

	incoming, err := parseTalosconfig(data)
	if err != nil {
		return nil, err
	}
	src := extractSrcContext(incoming)
	if src == nil {
		return nil, fmt.Errorf("incoming talosconfig has no usable context")
	}
	if len(endpoints) > 0 {
		src.Endpoints = dedupeKeepOrder(endpoints)
	}

	if targetPath == "" {
		targetPath = defaultTalosconfigPath()
	}
	target, err := loadOrNewTalosconfig(targetPath)
	if err != nil {
		return nil, err
	}

	overwrote := false
	if _, exists := target.Contexts[contextName]; exists {
		if !force {
			return nil, fmt.Errorf("context %q already exists in %s (use --force to overwrite)", contextName, targetPath)
		}
		overwrote = true
	}
	target.Contexts[contextName] = src
	if activate {
		target.Context = contextName
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating talosconfig dir: %w", err)
	}
	if err := writeTalosconfig(targetPath, target); err != nil {
		return nil, err
	}
	return &TalosconfigOutcome{
		Path:      targetPath,
		Context:   contextName,
		Endpoints: src.Endpoints,
		Activated: activate,
		Overwrote: overwrote,
	}, nil
}

// WriteTalosconfigFile renames the incoming talosconfig's sole context to
// `contextName` (overriding endpoints if provided) and writes it to
// `path` verbatim.
func WriteTalosconfigFile(data []byte, contextName string, endpoints []string, path string) error {
	if len(data) == 0 {
		return fmt.Errorf("empty talosconfig")
	}
	cfg, err := parseTalosconfig(data)
	if err != nil {
		return err
	}
	src := extractSrcContext(cfg)
	if src == nil {
		return fmt.Errorf("talosconfig has no usable context")
	}
	if len(endpoints) > 0 {
		src.Endpoints = dedupeKeepOrder(endpoints)
	}
	out := &talosConfig{
		Context:  contextName,
		Contexts: map[string]*talosCtxEntry{contextName: src},
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}
	return writeTalosconfig(path, out)
}

func defaultTalosconfigPath() string {
	if env := os.Getenv("TALOSCONFIG"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".talos/config"
	}
	return filepath.Join(home, ".talos", "config")
}

func parseTalosconfig(data []byte) (*talosConfig, error) {
	var cfg talosConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing talosconfig: %w", err)
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]*talosCtxEntry{}
	}
	return &cfg, nil
}

func loadOrNewTalosconfig(path string) (*talosConfig, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return &talosConfig{Contexts: map[string]*talosCtxEntry{}}, nil
	} else if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("talosconfig path %s is a directory", path)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- resolved from TALOSCONFIG or ~/.talos/config, not attacker-controlled
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return parseTalosconfig(data)
}

func writeTalosconfig(path string, cfg *talosConfig) error {
	// Sort contexts for stable output.
	names := make([]string, 0, len(cfg.Contexts))
	for k := range cfg.Contexts {
		names = append(names, k)
	}
	sort.Strings(names)

	out := map[string]any{}
	if cfg.Context != "" {
		out["context"] = cfg.Context
	}
	ctxs := map[string]any{}
	for _, name := range names {
		entry := cfg.Contexts[name]
		ctxs[name] = talosEntryToMap(entry)
	}
	out["contexts"] = ctxs

	data, err := yaml.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshalling talosconfig: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

func talosEntryToMap(e *talosCtxEntry) map[string]any {
	m := map[string]any{}
	if len(e.Endpoints) > 0 {
		m["endpoints"] = e.Endpoints
	}
	if len(e.Nodes) > 0 {
		m["nodes"] = e.Nodes
	}
	if e.CA != "" {
		m["ca"] = e.CA
	}
	if e.Crt != "" {
		m["crt"] = e.Crt
	}
	if e.Key != "" {
		m["key"] = e.Key
	}
	for k, v := range e.Extra {
		m[k] = v
	}
	return m
}

func extractSrcContext(cfg *talosConfig) *talosCtxEntry {
	if cfg == nil || len(cfg.Contexts) == 0 {
		return nil
	}
	name := cfg.Context
	if name == "" || cfg.Contexts[name] == nil {
		// Fall back to the first context alphabetically (stable).
		names := make([]string, 0, len(cfg.Contexts))
		for k := range cfg.Contexts {
			names = append(names, k)
		}
		sort.Strings(names)
		name = names[0]
	}
	return cfg.Contexts[name]
}

func dedupeKeepOrder(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
