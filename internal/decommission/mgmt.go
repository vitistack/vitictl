package decommission

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
)

// teardown deletes the KubernetesCluster CR and watches the operator-driven
// teardown until everything is verifiably gone. Finalizers
// (kubernetescluster.vitistack.io/finalizer, machine.vitistack.io/finalizer)
// are the teardown mechanism — never touched. On timeout the remains are
// reported; the fix is the vitistack operator's logs, not finalizer removal.
func (r *Runner) teardown(ctx context.Context) error {
	r.printf("Phase 2: delete KubernetesCluster %s/%s (clusterId %s) — IRREVERSIBLE", r.namespace, r.cluster.Name, r.clusterID)

	if err := r.mgmt.Delete(ctx, r.cluster); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting KubernetesCluster: %w", err)
	}

	// VM teardown is the slow part; machines cascade via ownerReferences and
	// each Machine's finalizer dismantles its VM on the provider.
	if !r.waitUntil(ctx, "machines terminated (VM teardown)", r.opts.MachineTimeout, 10*time.Second, func(ctx context.Context) (bool, error) {
		n, err := r.countClusterMachines(ctx)
		return n == 0, err
	}) {
		r.failed = true
	}
	if !r.waitUntil(ctx, "networkconfigurations removed", 2*time.Minute, 5*time.Second, func(ctx context.Context) (bool, error) {
		var l vitiv1alpha1.NetworkConfigurationList
		if err := r.mgmt.List(ctx, &l, ctrlclient.InNamespace(r.namespace)); err != nil {
			return false, err
		}
		for i := range l.Items {
			if r.ownsNetworkConfiguration(&l.Items[i]) {
				return false, nil
			}
		}
		return true, nil
	}) {
		r.failed = true
	}
	// The cpvip has NO ownerReference to the cluster (associated by name
	// only) — verify explicitly: it holds the API-server VIP, an external IP
	// allocation that would otherwise leak silently.
	if !r.waitUntil(ctx, "controlplanevirtualsharedip removed (API VIP)", 5*time.Minute, 5*time.Second, func(ctx context.Context) (bool, error) {
		var vip vitiv1alpha1.ControlPlaneVirtualSharedIP
		err := r.mgmt.Get(ctx, ctrlclient.ObjectKey{Namespace: r.namespace, Name: r.clusterID}, &vip)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}) {
		r.failed = true
	}
	if !r.waitUntil(ctx, "KubernetesCluster CR gone (finalizer released)", 5*time.Minute, 5*time.Second, func(ctx context.Context) (bool, error) {
		var kc vitiv1alpha1.KubernetesCluster
		err := r.mgmt.Get(ctx, ctrlclient.ObjectKey{Namespace: r.namespace, Name: r.cluster.Name}, &kc)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}) {
		r.failed = true
	}

	// Node-IP release: ground truth is the IPAllocation records. The
	// NetworkNamespace's status summary is NOT reliable (goes stale after
	// deletion — known cosmetic operator bug) and is deliberately ignored.
	// The CRD only exists where the static-ip-operator is rolled out.
	leaked, supported, err := r.remainingIPAllocations(ctx)
	switch {
	case err != nil:
		r.failf("could not verify IPAllocations: %v", err)
	case !supported:
		r.printf("  IPAllocation CRD not installed on this zone (static IP allocation not enabled here) — node-IP check skipped")
	case len(leaked) > 0:
		for _, name := range leaked {
			r.failf("node-IP allocation still present (real leak): %s", name)
		}
	default:
		r.printf("  node-IP allocations released (no IPAllocation records remain for %s)", r.clusterID)
	}

	return r.verifyTeardown(ctx)
}

func (r *Runner) countClusterMachines(ctx context.Context) (int, error) {
	var l vitiv1alpha1.MachineList
	if err := r.mgmt.List(ctx, &l, ctrlclient.InNamespace(r.namespace)); err != nil {
		return -1, err
	}
	n := 0
	for i := range l.Items {
		name := l.Items[i].Name
		if r.hasClusterNodeName(name, false) {
			n++
		} else if strings.HasPrefix(name, r.clusterID+"-") {
			return -1, fmt.Errorf("machine %q starts with clusterId %q but has an unknown name shape; ownership cannot be verified", name, r.clusterID)
		}
	}
	return n, nil
}

func (r *Runner) ownsNetworkConfiguration(nc *vitiv1alpha1.NetworkConfiguration) bool {
	if nc.Spec.ClusterIdentifier != "" {
		return nc.Spec.ClusterIdentifier == r.clusterID
	}
	if r.hasClusterNodeName(nc.Name, false) || r.hasClusterNodeName(nc.Spec.Name, false) {
		return true
	}
	for _, owner := range nc.OwnerReferences {
		if owner.Kind == "Machine" && r.hasClusterNodeName(owner.Name, false) {
			return true
		}
	}
	return false
}

// remainingIPAllocations returns the cluster's leftover IPAllocation names.
// supported is false only when the CRD is not installed on this zone.
func (r *Runner) remainingIPAllocations(ctx context.Context) (leaked []string, supported bool, err error) {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(kube.IPAllocationListGVK)
	if listErr := r.mgmt.List(ctx, l, ctrlclient.InNamespace(r.namespace)); listErr != nil {
		if meta.IsNoMatchError(listErr) {
			return nil, false, nil // CRD absent: static IP allocation is not enabled on this zone
		}
		return nil, true, listErr
	}
	for i := range l.Items {
		name := l.Items[i].GetName()
		if r.hasClusterNodeName(name, true) {
			leaked = append(leaked, name)
		} else if strings.HasPrefix(name, r.clusterID+"-") {
			return nil, true, fmt.Errorf("IPAllocation %q starts with clusterId %q but has an unknown name shape; ownership cannot be verified", name, r.clusterID)
		}
	}
	return leaked, true, nil
}

// verifyTeardown re-checks everything and renders the final verdict.
func (r *Runner) verifyTeardown(ctx context.Context) error {
	var problems []string

	if n, err := r.countClusterMachines(ctx); err != nil {
		problems = append(problems, fmt.Sprintf("cannot verify machines: %v", err))
	} else if n > 0 {
		problems = append(problems, fmt.Sprintf("%d machine(s) remain", n))
	}
	var kc vitiv1alpha1.KubernetesCluster
	if err := r.mgmt.Get(ctx, ctrlclient.ObjectKey{Namespace: r.namespace, Name: r.cluster.Name}, &kc); err == nil {
		problems = append(problems, "KubernetesCluster CR still present")
	} else if !apierrors.IsNotFound(err) {
		problems = append(problems, fmt.Sprintf("cannot verify KubernetesCluster: %v", err))
	}
	var vip vitiv1alpha1.ControlPlaneVirtualSharedIP
	if err := r.mgmt.Get(ctx, ctrlclient.ObjectKey{Namespace: r.namespace, Name: r.clusterID}, &vip); err == nil {
		problems = append(problems, "ControlPlaneVirtualSharedIP still present (API VIP not released)")
	} else if !apierrors.IsNotFound(err) {
		problems = append(problems, fmt.Sprintf("cannot verify cpvip: %v", err))
	}

	if r.failed || len(problems) > 0 {
		for _, p := range problems {
			r.printf("  ❌ %s", p)
		}
		return fmt.Errorf("teardown NOT CLEAN — do NOT strip finalizers; investigate the vitistack operator logs on the management cluster")
	}
	r.printf("✅ Guest cluster %s fully deleted from Vitistack (%s)", r.cluster.Name, r.namespace)
	return nil
}
