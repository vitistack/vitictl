// Package login turns a KubernetesCluster's stored credentials secret into
// a ready-to-use kubectl + talosctl setup, either by merging into the
// user's default configs or by writing dedicated files per cluster.
package login

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// KubeconfigOutcome reports where a kubeconfig was written and under which
// context.
type KubeconfigOutcome struct {
	Path      string
	Context   string
	Activated bool
	Overwrote bool
}

// MergeKubeconfig merges the incoming kubeconfig bytes into the kubeconfig
// at `targetPath`, renaming the cluster/user/context to `contextName` so
// they don't collide with existing entries. If `targetPath` is empty, the
// user's default kubeconfig is resolved ($KUBECONFIG → ~/.kube/config).
//
// If `activate` is true the merged context becomes current-context.
// If `force` is false and a context with the same name already exists,
// the merge is refused with an error.
func MergeKubeconfig(data []byte, contextName, targetPath string, activate, force bool) (*KubeconfigOutcome, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty kubeconfig")
	}
	if contextName == "" {
		return nil, fmt.Errorf("context name is required")
	}

	incoming, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("parsing incoming kubeconfig: %w", err)
	}
	if len(incoming.Contexts) == 0 {
		return nil, fmt.Errorf("incoming kubeconfig has no contexts")
	}

	// Pick the incoming current-context; fall back to the first one.
	srcCtxName := incoming.CurrentContext
	if srcCtxName == "" || incoming.Contexts[srcCtxName] == nil {
		for k := range incoming.Contexts {
			srcCtxName = k
			break
		}
	}
	srcCtx := incoming.Contexts[srcCtxName]
	srcCluster := incoming.Clusters[srcCtx.Cluster]
	srcAuth := incoming.AuthInfos[srcCtx.AuthInfo]
	if srcCluster == nil || srcAuth == nil {
		return nil, fmt.Errorf("incoming kubeconfig context %q references missing cluster or user", srcCtxName)
	}

	if targetPath == "" {
		targetPath = defaultKubeconfigPath()
	}
	target, err := loadOrNewKubeconfig(targetPath)
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

	target.Clusters[contextName] = srcCluster
	target.AuthInfos[contextName] = srcAuth
	target.Contexts[contextName] = &clientcmdapi.Context{
		Cluster:  contextName,
		AuthInfo: contextName,
	}
	if activate {
		target.CurrentContext = contextName
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating kubeconfig dir: %w", err)
	}
	if err := clientcmd.WriteToFile(*target, targetPath); err != nil {
		return nil, fmt.Errorf("writing kubeconfig: %w", err)
	}
	return &KubeconfigOutcome{
		Path:      targetPath,
		Context:   contextName,
		Activated: activate,
		Overwrote: overwrote,
	}, nil
}

// WriteKubeconfigFile renames the incoming kubeconfig's context to
// `contextName`, makes it current, and writes it to `path` verbatim
// (no merging with an existing file).
func WriteKubeconfigFile(data []byte, contextName, path string) error {
	if len(data) == 0 {
		return fmt.Errorf("empty kubeconfig")
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return fmt.Errorf("parsing kubeconfig: %w", err)
	}

	// Normalise: rename the single context + its cluster/user to contextName
	// so downstream tools see a clean "<clusterId>" entry.
	srcCtxName := cfg.CurrentContext
	if srcCtxName == "" || cfg.Contexts[srcCtxName] == nil {
		for k := range cfg.Contexts {
			srcCtxName = k
			break
		}
	}
	srcCtx := cfg.Contexts[srcCtxName]
	srcCluster := cfg.Clusters[srcCtx.Cluster]
	srcAuth := cfg.AuthInfos[srcCtx.AuthInfo]
	if srcCluster == nil || srcAuth == nil {
		return fmt.Errorf("kubeconfig context %q references missing cluster or user", srcCtxName)
	}

	cfg.Clusters = map[string]*clientcmdapi.Cluster{contextName: srcCluster}
	cfg.AuthInfos = map[string]*clientcmdapi.AuthInfo{contextName: srcAuth}
	cfg.Contexts = map[string]*clientcmdapi.Context{
		contextName: {Cluster: contextName, AuthInfo: contextName},
	}
	cfg.CurrentContext = contextName

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}
	return clientcmd.WriteToFile(*cfg, path)
}

// defaultKubeconfigPath mirrors clientcmd's loading-rule precedence:
// $KUBECONFIG (first entry if multiple), else ~/.kube/config.
func defaultKubeconfigPath() string {
	if env := os.Getenv("KUBECONFIG"); env != "" {
		// clientcmd splits on os.PathListSeparator; we pick the first entry
		// for writes, which matches the expected "primary" file.
		for _, p := range filepath.SplitList(env) {
			if p != "" {
				return p
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".kube/config"
	}
	return filepath.Join(home, ".kube", "config")
}

func loadOrNewKubeconfig(path string) (*clientcmdapi.Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return clientcmdapi.NewConfig(), nil
	} else if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading existing kubeconfig %s: %w", path, err)
	}
	if cfg.Clusters == nil {
		cfg.Clusters = map[string]*clientcmdapi.Cluster{}
	}
	if cfg.AuthInfos == nil {
		cfg.AuthInfos = map[string]*clientcmdapi.AuthInfo{}
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]*clientcmdapi.Context{}
	}
	return cfg, nil
}
