package login

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"
)

// MergeChange is one entry a verbatim merge wrote into the target.
type MergeChange struct {
	Kind     string // "cluster", "user" or "context"
	Name     string
	Replaced bool // an entry of the same name existed and was overwritten
}

// MergeReport describes what a verbatim merge did to the target file.
type MergeReport struct {
	Path           string // the file written, after following symlinks
	Backup         string // copy of the previous file; empty if there was none
	Changes        []MergeChange
	CurrentContext string // current context after the merge
}

// MergeKubeconfigVerbatim merges every cluster, user and context of a
// kubeconfig someone handed over into the kubeconfig at targetPath ("" means
// $KUBECONFIG's first entry, else ~/.kube/config), keeping their names.
//
// Unlike `kubectl config view --flatten` over several files, where the
// existing entry wins, the incoming entry replaces a same-named one: a
// re-fetched kubeconfig exists to heal an expired one. The target's
// current-context is kept, and the previous file is backed up first.
func MergeKubeconfigVerbatim(data []byte, targetPath string) (*MergeReport, error) {
	incoming, err := clientcmd.Load(data)
	if err != nil {
		if _, ctxs, terr := parseTalosMap(data); terr == nil && len(ctxs) > 0 {
			return nil, fmt.Errorf("this looks like a talosconfig, not a kubeconfig")
		}
		return nil, fmt.Errorf("parsing incoming kubeconfig: %w", err)
	}
	if len(incoming.Clusters)+len(incoming.AuthInfos)+len(incoming.Contexts) == 0 {
		return nil, fmt.Errorf("not a kubeconfig: no clusters, users or contexts")
	}
	// Paths in a handed-over file point at the sender's disk; embed them so
	// the merged entry is self-contained, as `kubectl config view --flatten`.
	if err := clientcmdapi.FlattenConfig(incoming); err != nil {
		return nil, fmt.Errorf("embedding files the kubeconfig references: %w", err)
	}

	if targetPath == "" {
		targetPath = defaultKubeconfigPath()
	}
	path, old, err := readTarget(targetPath)
	if err != nil {
		return nil, err
	}
	target := clientcmdapi.NewConfig()
	if old != nil {
		// Load, not LoadFromFile: the latter rewrites relative paths in the
		// user's own entries to absolute ones.
		if target, err = clientcmd.Load(old); err != nil {
			return nil, fmt.Errorf("parsing existing kubeconfig %s: %w", path, err)
		}
	}
	if target.Clusters == nil {
		target.Clusters = map[string]*clientcmdapi.Cluster{}
	}
	if target.AuthInfos == nil {
		target.AuthInfos = map[string]*clientcmdapi.AuthInfo{}
	}
	if target.Contexts == nil {
		target.Contexts = map[string]*clientcmdapi.Context{}
	}

	rep := &MergeReport{Path: path}
	mergeNamed(rep, "cluster", target.Clusters, incoming.Clusters)
	mergeNamed(rep, "user", target.AuthInfos, incoming.AuthInfos)
	mergeNamed(rep, "context", target.Contexts, incoming.Contexts)
	if target.CurrentContext == "" {
		target.CurrentContext = incoming.CurrentContext
	}
	rep.CurrentContext = target.CurrentContext

	out, err := clientcmd.Write(*target)
	if err != nil {
		return nil, fmt.Errorf("serialising kubeconfig: %w", err)
	}
	if rep.Backup, err = replaceFile(path, old, out); err != nil {
		return nil, err
	}
	return rep, nil
}

// MergeTalosconfigVerbatim is MergeKubeconfigVerbatim for talosconfig: every
// incoming context replaces a same-named one, the target's current context is
// kept, and fields viti does not model are carried over untouched.
// targetPath "" means $TALOSCONFIG, else ~/.talos/config.
func MergeTalosconfigVerbatim(data []byte, targetPath string) (*MergeReport, error) {
	incoming, inCtxs, err := parseTalosMap(data)
	if err != nil {
		return nil, fmt.Errorf("parsing incoming talosconfig: %w", err)
	}
	if len(inCtxs) == 0 {
		return nil, fmt.Errorf("not a talosconfig: no contexts")
	}

	if targetPath == "" {
		targetPath = defaultTalosconfigPath()
	}
	path, old, err := readTarget(targetPath)
	if err != nil {
		return nil, err
	}
	target, ctxs := map[string]any{}, map[string]any{}
	if old != nil {
		if target, ctxs, err = parseTalosMap(old); err != nil {
			return nil, fmt.Errorf("parsing existing talosconfig %s: %w", path, err)
		}
	}

	rep := &MergeReport{Path: path}
	mergeNamed(rep, "context", ctxs, inCtxs)
	target["contexts"] = ctxs
	if cur, _ := target["context"].(string); cur == "" {
		if in, _ := incoming["context"].(string); in != "" {
			target["context"] = in
		}
	}
	rep.CurrentContext, _ = target["context"].(string)

	out, err := yaml.Marshal(target)
	if err != nil {
		return nil, fmt.Errorf("serialising talosconfig: %w", err)
	}
	if rep.Backup, err = replaceFile(path, old, out); err != nil {
		return nil, err
	}
	return rep, nil
}

// parseTalosMap reads a talosconfig generically, so unknown fields survive a
// round trip. A kubeconfig fails here: its contexts are a list, not a map.
func parseTalosMap(data []byte) (doc, contexts map[string]any, err error) {
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	contexts = map[string]any{}
	if raw := doc["contexts"]; raw != nil {
		var ok bool
		if contexts, ok = raw.(map[string]any); !ok {
			if _, isList := raw.([]any); isList {
				return nil, nil, fmt.Errorf("this looks like a kubeconfig, not a talosconfig")
			}
			return nil, nil, fmt.Errorf("contexts is not a map of named contexts")
		}
	}
	return doc, contexts, nil
}

func mergeNamed[T any](rep *MergeReport, kind string, dst, src map[string]T) {
	for _, name := range slices.Sorted(maps.Keys(src)) {
		_, exists := dst[name]
		dst[name] = src[name]
		rep.Changes = append(rep.Changes, MergeChange{Kind: kind, Name: name, Replaced: exists})
	}
}

// readTarget resolves a symlinked config to the file it points at, so the
// write replaces that file rather than the link, and returns its content (nil
// when it does not exist yet).
func readTarget(path string) (string, []byte, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if path, err = filepath.EvalSymlinks(path); err != nil {
			return "", nil, fmt.Errorf("resolving symlink: %w", err)
		}
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the user's own config path
	if errors.Is(err, fs.ErrNotExist) {
		return path, nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return path, data, nil
}

// replaceFile backs up old (when non-nil) and swaps data in with a rename in
// the same directory, so a failure never leaves a half-written config.
func replaceFile(path string, old, data []byte) (backup string, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	if old != nil {
		if backup, err = writeBackup(path, old); err != nil {
			return "", err
		}
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Chmod(0o600)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("replacing %s: %w", path, err)
	}
	return backup, nil
}

// writeBackup writes <path>.bak-<timestamp>, the naming the team's shell
// helpers use, adding -1, -2 … rather than overwriting an earlier backup
// from the same second.
func writeBackup(path string, data []byte) (string, error) {
	base := path + ".bak-" + time.Now().Format("2006-01-02-150405")
	for i := range 100 {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- derived from the user's own config path
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("creating backup: %w", err)
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(name)
			return "", fmt.Errorf("writing backup %s: %w", name, err)
		}
		return name, nil
	}
	return "", fmt.Errorf("too many backups named %s-*", base)
}
