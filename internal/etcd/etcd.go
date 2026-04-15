// Package etcd dispatches provider-specific etcd snapshot and restore
// operations for KubernetesClusters. Each provider self-registers a
// Handler in its own file.
package etcd

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
)

// SnapshotRequest is the input the cobra command builds and hands to a
// provider handler for `viti kc etcd-backup`.
type SnapshotRequest struct {
	Cluster   *vitiv1alpha1.KubernetesCluster
	Secret    *corev1.Secret
	Client    *kube.Client
	Endpoints []string // cluster CP endpoints (cert-valid, for talosctl --endpoints)
	Node      string   // single CP node to snapshot from
	Output    string   // local file path to write the snapshot to
	CopyRaw   bool     // use the unhealthy-cluster fallback (cp /var/lib/etcd/...)
}

// RestoreRequest is the input for `viti kc etcd-restore`.
type RestoreRequest struct {
	Cluster       *vitiv1alpha1.KubernetesCluster
	Secret        *corev1.Secret
	Client        *kube.Client
	Endpoints     []string // cluster CP endpoints
	Node          string   // single CP node to bootstrap-restore against
	Input         string   // local snapshot file
	SkipHashCheck bool     // for raw-copied snapshots (no integrity hash)
}

// Handler dispatches snapshot/restore operations per provider.
type Handler interface {
	Snapshot(ctx context.Context, req SnapshotRequest) error
	Restore(ctx context.Context, req RestoreRequest) error
}

var handlers = map[vitiv1alpha1.KubernetesProviderType]Handler{}

// Register installs a handler for a provider, called from each provider
// file's init().
func Register(pt vitiv1alpha1.KubernetesProviderType, h Handler) {
	handlers[pt] = h
}

// ForProvider returns the handler for the given provider, or an error if
// none is registered.
func ForProvider(pt vitiv1alpha1.KubernetesProviderType) (Handler, error) {
	if pt == "" {
		return nil, fmt.Errorf("cluster has no provider set on spec.data.provider")
	}
	h, ok := handlers[pt]
	if !ok {
		return nil, fmt.Errorf("etcd snapshot/restore is not implemented for provider %q", pt)
	}
	return h, nil
}
