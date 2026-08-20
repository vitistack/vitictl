package netns

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
)

// Load snapshots one availability zone with a single List per resource type.
// namespace == "" loads all namespaces. Any List failure is an error —
// except the ipallocations kind not existing on the zone, which is recorded
// as IPAllocCRDPresent=false.
func Load(ctx context.Context, c ctrlclient.Client, namespace string) (*Snapshot, error) {
	var opts []ctrlclient.ListOption
	if namespace != "" {
		opts = append(opts, ctrlclient.InNamespace(namespace))
	}

	s := &Snapshot{}

	var nns vitiv1alpha1.NetworkNamespaceList
	if err := c.List(ctx, &nns, opts...); err != nil {
		return nil, fmt.Errorf("listing networknamespaces: %w", err)
	}
	s.NetNSs = nns.Items

	var kcs vitiv1alpha1.KubernetesClusterList
	if err := c.List(ctx, &kcs, opts...); err != nil {
		return nil, fmt.Errorf("listing kubernetesclusters: %w", err)
	}
	s.KCs = kcs.Items

	var ncs vitiv1alpha1.NetworkConfigurationList
	if err := c.List(ctx, &ncs, opts...); err != nil {
		return nil, fmt.Errorf("listing networkconfigurations: %w", err)
	}
	s.NCs = ncs.Items

	ias := &unstructured.UnstructuredList{}
	ias.SetGroupVersionKind(kube.IPAllocationListGVK)
	err := c.List(ctx, ias, opts...)
	switch {
	case err == nil:
		s.IPAllocs = ias.Items
		s.IPAllocCRDPresent = true
	case meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err):
		// static-ip-operator CRD not rolled out on this zone (e.g. ptr1/bgo)
		s.IPAllocCRDPresent = false
	default:
		return nil, fmt.Errorf("listing ipallocations: %w", err)
	}
	return s, nil
}
