package login

import (
	"context"
	"fmt"
	"net"
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
//  2. Machine objects named <clusterId>-ctp* — their node IP addresses.
//
// The default (includeVIP=false) targets the control-plane nodes directly,
// which is what most on-prem / tunnel-connected operators want: the VIP
// adds a routing hop and can be unreachable from networks where the CP
// node IPs are. Callers that explicitly want the load-balancer address
// pass includeVIP=true.
//
// includeIPv6 controls whether IPv6 addresses are included in the result.
// Default false because operator workstations often can't reach v6 endpoints
// (no v6 transit, NAT64 not in path) and a single unreachable v6 entry will
// hang `talosctl` until its dial timeout. Pass true to opt in.
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
	includeIPv6 bool,
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
	if filtered := filterIPFamily(cpvipAddrs, includeIPv6); len(filtered) > 0 {
		return dedupeKeepOrder(filtered), SourceCPVIP, warnings, nil
	}

	// 2. Machines named <clusterId>-ctp*
	mAddrs, mWarn, merr := controlPlaneMachineEndpoints(ctx, c, namespace, clusterID, includeIPv6)
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

func controlPlaneMachineEndpoints(ctx context.Context, c ctrlclient.Client, namespace, clusterID string, includeIPv6 bool) ([]string, string, error) {
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
		addrs = append(addrs, filterIPFamily(MachineNodeIPs(m), includeIPv6)...)
	}
	if seen == 0 {
		return nil, fmt.Sprintf("no Machines matched %s* in namespace %s", prefix, namespace), nil
	}
	if len(addrs) == 0 {
		return nil, fmt.Sprintf("found %d control-plane machine(s) matching %s* but none have a usable node IP", seen, prefix), nil
	}
	return dedupeKeepOrder(addrs), "", nil
}

// filterIPFamily returns only the IPv4 entries from in when includeIPv6 is
// false; returns in unchanged otherwise. Default behaviour is v4-only because
// many operator workstations can't reach v6 endpoints (no v6 transit, no
// NAT64, etc.) and a single unreachable v6 entry will hang `talosctl` until
// its dial timeout fires. Pass includeIPv6=true to opt back in.
func filterIPFamily(in []string, includeIPv6 bool) []string {
	if includeIPv6 {
		return in
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		if ip.To4() == nil {
			continue
		}
		out = append(out, s)
	}
	return out
}

// ResolveClusterMachineNodes returns the IP addresses of every Machine that
// belongs to the cluster (control planes AND workers). It is the right input
// for `talosctl dashboard --nodes` when the operator wants visibility into
// the whole cluster — the API endpoint set (control-plane only) is resolved
// separately by ResolveControlPlaneEndpoints.
//
// Filter is by name prefix "<clusterId>-", matching the vitistack naming
// scheme used by the talos-operator (<clusterId>-ctpN, <clusterId>-wrkN…).
// Returns the addresses, a warning suitable for surfacing to the user when
// non-fatal, and an error for hard failures (e.g. List failed).
func ResolveClusterMachineNodes(ctx context.Context, c ctrlclient.Client, namespace, clusterID string, includeIPv6 bool) ([]string, string, error) {
	if clusterID == "" {
		return nil, "", fmt.Errorf("clusterId is empty")
	}
	var list vitiv1alpha1.MachineList
	if err := c.List(ctx, &list, ctrlclient.InNamespace(namespace)); err != nil {
		return nil, "", fmt.Errorf("listing Machines: %w", err)
	}
	prefix := clusterID + "-"
	var addrs []string
	var seen int
	for i := range list.Items {
		m := &list.Items[i]
		if !strings.HasPrefix(m.Name, prefix) {
			continue
		}
		seen++
		addrs = append(addrs, filterIPFamily(MachineNodeIPs(m), includeIPv6)...)
	}
	if seen == 0 {
		return nil, fmt.Sprintf("no Machines matched %s* in namespace %s", prefix, namespace), nil
	}
	if len(addrs) == 0 {
		return nil, fmt.Sprintf("found %d machine(s) matching %s* but none have a usable node IP", seen, prefix), nil
	}
	return dedupeKeepOrder(addrs), "", nil
}

// MachineNodeIPs returns the deduped list of node-level IP addresses for m.
// "Node-level" means addresses that belong to a real NIC — not pod CNI
// bridges, kube-proxy dummy interfaces, per-pod veth pairs, or libvirt/ovs
// bridges. Same-order semantics: callers can index [0] to pick a primary IP.
//
// Resolution:
//  1. If Status.NetworkInterfaces is populated, it's the source of truth.
//     Two filtering modes are used:
//     a. Kubevirt: when at least one interface's Type carries the kubevirt
//     "attached" marker (`domain` or `multus-status`, copied verbatim
//     from the VMI's infoSource), only those interfaces are kept. The
//     other entries are guest-agent reports of interfaces the guest OS
//     happens to expose (CNI veths, cilium_host /32 from pod CIDR, …)
//     and have no Name set — the name-based filter cannot catch them.
//     b. Otherwise: drop interfaces whose name matches a known virtual
//     pattern (cni0, flannel.*, cilium_*, calico*, weave*, kube-ipvs0,
//     vethXXX, docker0, br-*, lo, …) and keep the rest.
//  2. Otherwise it falls back to Status.IPAddresses, then Private, then
//     Public — same as the previous behaviour. Loopback/link-local/
//     unspecified addresses are dropped regardless of source.
//
// Both IPv4 and IPv6 are returned; the Talos client and `talosctl` accept
// either as endpoints or nodes.
func MachineNodeIPs(m *vitiv1alpha1.Machine) []string {
	if m == nil {
		return nil
	}
	var raw []string
	if len(m.Status.NetworkInterfaces) > 0 {
		kubevirtMode := hasKubevirtAttachedInterface(m.Status.NetworkInterfaces)
		for _, iface := range m.Status.NetworkInterfaces {
			if kubevirtMode {
				if !isKubevirtAttachedInterface(iface) {
					continue
				}
			} else if isVirtualInterfaceName(iface.Name) {
				continue
			}
			raw = append(raw, iface.IPAddresses...)
			raw = append(raw, iface.IPv6Addresses...)
		}
	}
	if len(raw) == 0 {
		switch {
		case len(m.Status.IPAddresses) > 0:
			raw = append(raw, m.Status.IPAddresses...)
		case len(m.Status.PrivateIPAddresses) > 0:
			raw = append(raw, m.Status.PrivateIPAddresses...)
		case len(m.Status.PublicIPAddresses) > 0:
			raw = append(raw, m.Status.PublicIPAddresses...)
		}
	}
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if !isUsableNodeIP(s) {
			continue
		}
		out = append(out, s)
	}
	return dedupeKeepOrder(out)
}

// isUsableNodeIP returns true for parseable, non-loopback, non-link-local,
// non-unspecified IP literals — i.e. addresses we'd actually try to dial.
func isUsableNodeIP(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	return true
}

// hasKubevirtAttachedInterface reports whether ifaces contains at least one
// entry whose Type carries the kubevirt "attached" marker. See
// isKubevirtAttachedInterface for what that means.
func hasKubevirtAttachedInterface(ifaces []vitiv1alpha1.NetworkInterfaceStatus) bool {
	for i := range ifaces {
		if isKubevirtAttachedInterface(ifaces[i]) {
			return true
		}
	}
	return false
}

// isKubevirtAttachedInterface reports whether iface was actually attached by
// kubevirt, vs. merely visible to the qemu-guest-agent inside the guest OS.
//
// The kubevirt-operator copies vmi.Status.Interfaces[].InfoSource verbatim
// into NetworkInterfaceStatus.Type. InfoSource is a comma-separated list of
// where the info came from: `domain` means kubevirt defined the NIC in the
// libvirt domain XML, `multus-status` means Multus reported a CNI attachment,
// and `guest-agent` means the guest OS reported the interface. NICs that are
// truly attached by kubevirt always carry `domain` and/or `multus-status`;
// `guest-agent`-only entries are the guest's own internal interfaces (CNI
// veths, cilium_host /32 from the pod CIDR, tunnels, …) which we must drop.
//
// Other providers don't set these markers (proxmox-operator sets `ethernet`),
// so callers should only use this filter when at least one interface in the
// machine carries one of the markers.
func isKubevirtAttachedInterface(iface vitiv1alpha1.NetworkInterfaceStatus) bool {
	t := strings.ToLower(iface.Type)
	return strings.Contains(t, "domain") || strings.Contains(t, "multus-status")
}

// isVirtualInterfaceName matches the names of well-known interfaces that
// don't carry a node's primary IP: CNI bridges (cni0), flannel/cilium/calico
// dataplane interfaces, kube-proxy's dummy interface, per-pod vethXXX
// devices, container engine bridges (docker0, br-*), and so on. Names that
// don't match are kept (we'd rather include an unknown NIC than drop one).
func isVirtualInterfaceName(name string) bool {
	if name == "" {
		return false
	}
	n := strings.ToLower(name)
	switch n {
	case "lo", "cni0", "docker0", "kube-bridge", "kube-ipvs0", "kube-dummy-if",
		"tunl0", "ip6tnl0", "sit0", "gretap0", "erspan0":
		return true
	}
	for _, prefix := range []string{
		"flannel.",   // flannel.1, flannel.2 …
		"cilium_",    // cilium_host, cilium_net, cilium_vxlan
		"cilium-",    // cilium-health, cilium-tap
		"lxc",        // cilium per-pod lxcXXX
		"calico",     // calico*, calixxx, vxlan.calico
		"vxlan.cali", // calico vxlan device
		"weave",      // weave, weave-bridge
		"veth",       // per-pod veth pairs
		"br-",        // docker / containerd bridges
		"dummy",      // dummyN
		"ovs-",       // open vswitch
		"vnet",       // libvirt/QEMU host-side tap names (vnet0, vnet1, …)
	} {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}
