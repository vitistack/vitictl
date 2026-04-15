package etcd

import (
	"context"
	"fmt"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

type aksHandler struct{}

func init() {
	Register(vitiv1alpha1.KubernetesProviderTypeAKS, &aksHandler{})
}

// AKS hides etcd behind the managed control plane — Azure runs and
// backs it up; users don't have direct access. We surface that fact
// rather than pretending it can be done from this CLI.

func (a *aksHandler) Snapshot(_ context.Context, _ SnapshotRequest) error {
	return fmt.Errorf("etcd snapshot is not user-accessible on AKS — Azure manages etcd internally; use Azure Backup / cluster export tooling instead")
}

func (a *aksHandler) Restore(_ context.Context, _ RestoreRequest) error {
	return fmt.Errorf("etcd restore is not user-accessible on AKS — Azure manages cluster recovery; restore via the Azure portal or `az aks` tooling")
}
