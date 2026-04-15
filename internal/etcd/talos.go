package etcd

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/talos"
)

type talosHandler struct{}

func init() {
	Register(vitiv1alpha1.KubernetesProviderTypeTalos, &talosHandler{})
}

func (t *talosHandler) Snapshot(_ context.Context, req SnapshotRequest) error {
	cfgPath, cleanup, err := setupTalosconfig(req.Secret, req.Cluster, req.Endpoints)
	if err != nil {
		return err
	}
	defer cleanup()

	op := "etcd snapshot"
	run := func() error { return talos.EtcdSnapshot(cfgPath, req.Node, req.Output) }
	if req.CopyRaw {
		op = "raw etcd file copy"
		run = func() error { return talos.EtcdSnapshotRaw(cfgPath, req.Node, req.Output) }
	}

	_, _ = fmt.Fprintf(os.Stderr, "📸 %s — endpoints: %v, node: %s → %s\n", op, req.Endpoints, req.Node, req.Output)
	if err := run(); err != nil {
		return fmt.Errorf("%s failed: %w\n\n💡 if the node/endpoint is unreachable from this machine (e.g. i/o timeout), override with --endpoint <reachable-addr> and/or --node <addr>", op, err)
	}
	_, _ = fmt.Fprintf(os.Stderr, "✅ wrote %s\n", req.Output)
	if req.CopyRaw {
		_, _ = fmt.Fprintln(os.Stderr, "ℹ️  raw snapshot has no integrity hash — pass --skip-hash-check to viti kc etcd-restore")
	}
	return nil
}

func (t *talosHandler) Restore(_ context.Context, req RestoreRequest) error {
	cfgPath, cleanup, err := setupTalosconfig(req.Secret, req.Cluster, req.Endpoints)
	if err != nil {
		return err
	}
	defer cleanup()

	_, _ = fmt.Fprintf(os.Stderr, "♻️  bootstrap --recover-from %s on node %s\n", req.Input, req.Node)
	if err := talos.BootstrapRecover(cfgPath, req.Node, req.Input, req.SkipHashCheck); err != nil {
		return fmt.Errorf("bootstrap recover failed: %w", err)
	}
	_, _ = fmt.Fprintln(os.Stderr, "✅ recovery initiated — talosd will log restoration progress on the node")
	return nil
}

// setupTalosconfig writes a temporary talosconfig keyed by the cluster's
// clusterId with the provided endpoints, and returns the file path plus
// a cleanup func.
func setupTalosconfig(secret *corev1.Secret, cluster *vitiv1alpha1.KubernetesCluster, endpoints []string) (string, func(), error) {
	if !talos.HasTalosctl() {
		return "", nil, fmt.Errorf("talosctl not found on PATH — install it from https://www.talos.dev/latest/talos-guides/install/talosctl/")
	}
	clusterID := cluster.Spec.Cluster.ClusterId
	if clusterID == "" {
		return "", nil, fmt.Errorf("cluster %s/%s has no spec.data.clusterId", cluster.Namespace, cluster.Name)
	}
	return talos.WriteTempTalosconfig(secret, clusterID, endpoints)
}
