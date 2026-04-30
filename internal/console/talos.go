package console

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

func (t *talosHandler) ClusterConsole(_ context.Context, req ClusterRequest) error {
	clusterID := req.Cluster.Spec.Cluster.ClusterId
	if clusterID == "" {
		return fmt.Errorf("cluster %s/%s has no spec.data.clusterId",
			req.Cluster.Namespace, req.Cluster.Name)
	}
	if len(req.Endpoints) == 0 {
		return fmt.Errorf("no control-plane endpoints available for cluster %s — cannot start dashboard", clusterID)
	}
	// Default --nodes to every Machine in the cluster so the dashboard shows
	// workers too. Fall back to the API endpoints when the caller didn't
	// supply a node set (older callers, or clusters where the Machine list
	// was unreachable).
	nodes := req.Nodes
	if len(nodes) == 0 {
		nodes = req.Endpoints
	}
	return openTalosDashboard(req.Secret, clusterID, req.Endpoints, nodes)
}

func (t *talosHandler) MachineConsole(_ context.Context, req MachineRequest) error {
	if req.Cluster == nil || req.Secret == nil {
		return fmt.Errorf("machine %s has no resolvable owning cluster (cannot obtain talos PKI)", req.Machine.Name)
	}
	clusterID := req.Cluster.Spec.Cluster.ClusterId
	if clusterID == "" {
		return fmt.Errorf("owning cluster %s/%s has no spec.data.clusterId",
			req.Cluster.Namespace, req.Cluster.Name)
	}
	if len(req.Endpoints) == 0 {
		return fmt.Errorf("no cluster control-plane endpoints resolved for %s — cannot connect Talos API", clusterID)
	}
	if len(req.Nodes) == 0 {
		return fmt.Errorf("machine %s has no node address resolved", req.Machine.Name)
	}
	return openTalosDashboard(req.Secret, clusterID, req.Endpoints, req.Nodes)
}

// openTalosDashboard writes a temporary talosconfig for `contextName`
// with the given endpoints, then execs `talosctl dashboard --nodes …`
// attached to the caller's stdio. The temp config is cleaned up after
// the dashboard exits (or the process is signalled).
func openTalosDashboard(secret *corev1.Secret, contextName string, endpoints, nodes []string) error {
	if !talos.HasTalosctl() {
		return fmt.Errorf("talosctl not found on PATH — install it from https://www.talos.dev/latest/talos-guides/install/talosctl/")
	}
	cfgPath, cleanup, err := talos.WriteTempTalosconfig(secret, contextName, endpoints)
	if err != nil {
		return err
	}
	defer cleanup()
	_, _ = fmt.Fprintf(os.Stderr, "🖥️  talosctl dashboard (context %q) — endpoints: %v, nodes: %v\n", contextName, endpoints, nodes)
	return talos.RunDashboard(cfgPath, endpoints, nodes)
}
