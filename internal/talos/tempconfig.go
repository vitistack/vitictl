// Package talos holds low-level helpers for interacting with a Talos
// cluster from viti subcommands (console/dashboard today, likely
// reboot/etcdbackup/upgrade/… later). Higher-level orchestration that
// merges configs into the user's ~/.talos/config lives in internal/login.
package talos

import (
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"

	"github.com/vitistack/vitictl/internal/extract"
	"github.com/vitistack/vitictl/internal/login"
)

// WriteTempTalosconfig turns a cluster credentials secret into a
// self-contained talosconfig on disk, sized for a single talosctl
// invocation (no merging with the user's persistent ~/.talos/config).
//
// The caller is responsible for invoking the returned cleanup func once
// the child process has exited, which removes the temp directory and the
// config within.
//
// The context is named contextName. If endpoints is non-empty the source
// endpoints are replaced with it; otherwise the talosconfig's original
// endpoints are preserved.
func WriteTempTalosconfig(secret *corev1.Secret, contextName string, endpoints []string) (path string, cleanup func(), err error) {
	raw, ok := secret.Data[extract.KeyTalosconfig]
	if !ok || len(raw) == 0 {
		return "", nil, fmt.Errorf("secret %s/%s has no %q entry (not a Talos cluster?)",
			secret.Namespace, secret.Name, extract.KeyTalosconfig)
	}

	dir, err := os.MkdirTemp("", "viti-talos-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	path = filepath.Join(dir, "talosconfig")
	if err := login.WriteTalosconfigFile(raw, contextName, endpoints, path); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing temp talosconfig: %w", err)
	}
	return path, cleanup, nil
}
