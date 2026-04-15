package login

import (
	"context"
	"fmt"
	"strings"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

// EndpointSource names where a set of talos endpoints came from. Shown in
// the CLI output so the operator can tell at a glance which source was
// used and decide whether to override.
type EndpointSource string

const (
	SourceCPVIP    EndpointSource = "controlplanevirtualsharedip"
	SourceMachines EndpointSource = "machines"
	SourceOverride EndpointSource = "override"
	SourceSecret   EndpointSource = "talosconfig (from secret)" // #nosec G101 -- display label, not a credential
	SourceNone     EndpointSource = "none"
)

// ControlPlaneNameSuffix is the Machine-name fragment that identifies a
// control plane node in the vitistack naming scheme: <clusterId>-ctp<N>.
const ControlPlaneNameSuffix = "-ctp"

// ResolveControlPlaneEndpoints tries, in order:
//  1. the ControlPlaneVirtualSharedIP (CPVIP) matching the cluster — its
//     status.poolMembers, then spec.poolMembers as a fallback. When
//     includeVIP is true the VIP(s) from status.loadBalancerIps are
//     prepended to the result;
//  2. Machine objects named <clusterId>-ctp* — their status.ipAddresses.
//
// The default (includeVIP=false) targets the control-plane nodes directly,
// which is what most on-prem / tunnel-connected operators want: the VIP
// adds a routing hop and can be unreachable from networks where the CP
// node IPs are. Callers that explicitly want the load-balancer address
// pass includeVIP=true.
//
// Returns the collected addresses, the source that was used, and any
// warnings worth surfacing to the user. If nothing is found, returns
// (nil, SourceNone, warnings, nil) — caller should fall back to what the
// secret's talosconfig already contains.
func ResolveControlPlaneEndpoints(
	ctx context.Context,
	c ctrlclient.Client,
	namespace, clusterID string,
	includeVIP bool,
) (addrs []string, source EndpointSource, warnings []string, err error) {
	if clusterID == "" {
		return nil, SourceNone, nil, fmt.Errorf("clusterId is empty")
	}

	// 1. CPVIP
	cpvipAddrs, cpvipWarn, cerr := cpvipEndpoints(ctx, c, namespace, clusterID, includeVIP)
	if cerr != nil {
		warnings = append(warnings, fmt.Sprintf("CPVIP lookup: %v", cerr))
	}
	if cpvipWarn != "" {
		warnings = append(warnings, cpvipWarn)
	}
	if len(cpvipAddrs) > 0 {
		return dedupeKeepOrder(cpvipAddrs), SourceCPVIP, warnings, nil
	}

	// 2. Machines named <clusterId>-ctp*
	mAddrs, mWarn, merr := controlPlaneMachineEndpoints(ctx, c, namespace, clusterID)
	if merr != nil {
		warnings = append(warnings, fmt.Sprintf("machine lookup: %v", merr))
	}
	if mWarn != "" {
		warnings = append(warnings, mWarn)
	}
	if len(mAddrs) > 0 {
		return dedupeKeepOrder(mAddrs), SourceMachines, warnings, nil
	}
	return nil, SourceNone, warnings, nil
}

func cpvipEndpoints(ctx context.Context, c ctrlclient.Client, namespace, clusterID string, includeVIP bool) ([]string, string, error) {
	var list vitiv1alpha1.ControlPlaneVirtualSharedIPList
	if err := c.List(ctx, &list, ctrlclient.InNamespace(namespace)); err != nil {
		return nil, "", fmt.Errorf("listing ControlPlaneVirtualSharedIPs: %w", err)
	}
	var matched []vitiv1alpha1.ControlPlaneVirtualSharedIP
	for i := range list.Items {
		if list.Items[i].Spec.ClusterIdentifier == clusterID {
			matched = append(matched, list.Items[i])
		}
	}
	switch len(matched) {
	case 0:
		return nil, "", nil
	case 1:
		// ok
	default:
		names := make([]string, 0, len(matched))
		for _, m := range matched {
			names = append(names, m.Name)
		}
		return nil, fmt.Sprintf("multiple CPVIPs match clusterId %s: %s (using first)", clusterID, strings.Join(names, ", ")), nil
	}
	cpvip := matched[0]

	var addrs []string
	if includeVIP {
		addrs = append(addrs, cpvip.Status.LoadBalancerIps...)
	}
	addrs = append(addrs, cpvip.Status.PoolMembers...)
	if len(addrs) > 0 {
		return dedupeKeepOrder(addrs), "", nil
	}
	// Status empty — fall back to spec.poolMembers (desired state) so
	// newly-provisioned clusters aren't unusable just because the CPVIP
	// controller hasn't reconciled yet.
	addrs = append(addrs, cpvip.Spec.PoolMembers...)
	if len(addrs) > 0 {
		return dedupeKeepOrder(addrs), fmt.Sprintf("CPVIP %s has no status addresses yet; using spec.poolMembers", cpvip.Name), nil
	}
	return nil, fmt.Sprintf("CPVIP %s has no addresses populated", cpvip.Name), nil
}

func controlPlaneMachineEndpoints(ctx context.Context, c ctrlclient.Client, namespace, clusterID string) ([]string, string, error) {
	var list vitiv1alpha1.MachineList
	if err := c.List(ctx, &list, ctrlclient.InNamespace(namespace)); err != nil {
		return nil, "", fmt.Errorf("listing Machines: %w", err)
	}
	prefix := clusterID + ControlPlaneNameSuffix
	var addrs []string
	var seen int
	for i := range list.Items {
		m := &list.Items[i]
		if !strings.HasPrefix(m.Name, prefix) {
			continue
		}
		seen++
		// Prefer the general IP list; fall back to private, then public.
		switch {
		case len(m.Status.IPAddresses) > 0:
			addrs = append(addrs, m.Status.IPAddresses...)
		case len(m.Status.PrivateIPAddresses) > 0:
			addrs = append(addrs, m.Status.PrivateIPAddresses...)
		case len(m.Status.PublicIPAddresses) > 0:
			addrs = append(addrs, m.Status.PublicIPAddresses...)
		}
	}
	if seen == 0 {
		return nil, fmt.Sprintf("no Machines matched %s* in namespace %s", prefix, namespace), nil
	}
	if len(addrs) == 0 {
		return nil, fmt.Sprintf("found %d control-plane machine(s) matching %s* but none have status.ipAddresses populated", seen, prefix), nil
	}
	return addrs, "", nil
}
