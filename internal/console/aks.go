package console

import (
	"context"
	"fmt"
	"os"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

type aksHandler struct{}

func init() {
	Register(vitiv1alpha1.KubernetesProviderTypeAKS, &aksHandler{})
}

// ClusterConsole for AKS prints a minimal "how to access" message. The
// AKS REST / portal metadata needed for a deep-link (subscription, RG,
// tenant) isn't reliably present in the cluster secret today, so for
// now the useful behaviour is to nudge the user toward kubectl.
func (a *aksHandler) ClusterConsole(_ context.Context, req ClusterRequest) error {
	clusterID := req.Cluster.Spec.Cluster.ClusterId
	if clusterID == "" {
		clusterID = req.Cluster.Name
	}
	_, _ = fmt.Fprintf(os.Stdout, `🪟 AKS console is not yet available as a built-in viti dashboard.
To interact with this cluster:
  1. viti kc login %s       # installs the kubeconfig as context %q
  2. kubectl --context %s get nodes
  3. (optional) az aks browse --resource-group <rg> --name <name>
`, req.Cluster.Name, clusterID, clusterID)
	return nil
}

func (a *aksHandler) MachineConsole(_ context.Context, req MachineRequest) error {
	return fmt.Errorf("per-machine console is not supported for AKS machine %q", req.Machine.Name)
}
