package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/console"
	"github.com/vitistack/vitictl/internal/extract"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/login"
)

var (
	machineConsoleNamespace string
	machineConsoleEndpoints []string
	machineConsoleNodes     []string
	machineConsoleUseVIP    bool
)

// Machines are named <clusterId>-ctp<N> for control planes and
// <clusterId>-wrk<N> for workers in the vitistack naming scheme.
var machineNameSuffixes = []string{"-ctp", "-wrk"}

var machineConsoleCmd = &cobra.Command{
	Use:   "console <name>",
	Short: "Open the provider-native dashboard for a single Machine",
	Long: `Opens a per-node dashboard. For Talos machines this runs
"talosctl dashboard --endpoints <cluster-cps> --nodes <machine-ip>" using a
temporary talosconfig derived from the owning cluster's credentials secret.

The owning cluster is inferred from the machine's name (vitistack scheme:
<clusterId>-ctp<N> or <clusterId>-wrk<N>). Control-plane endpoints come from
the same CPVIP / ctp-machine resolver "kc console" uses, since those are the
cert-valid addresses for the Talos API. The target node is picked from the
machine's own IPs, preferring one that also appears in the CPVIP pool to
dodge TLS SAN mismatches.

Override either side with --endpoint (repeatable) or --node (repeatable).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		zones, err := kube.ResolveAvailabilityZones(AvailabilityZone())
		if err != nil {
			return err
		}
		clients, err := kube.ConnectAll(ctx, zones, true, warn)
		if err != nil {
			return err
		}

		hit, err := findMachineAcrossAZs(ctx, clients, args[0], machineConsoleNamespace)
		if err != nil {
			return err
		}

		clusterID, err := clusterIDFromMachineName(hit.machine.Name)
		if err != nil {
			return err
		}

		owning, err := findClusterByID(ctx, hit.client, clusterID, hit.machine.Namespace)
		if err != nil {
			return err
		}
		secret, err := extract.FindClusterSecret(ctx, hit.client.Ctrl, owning)
		if err != nil {
			return err
		}

		handler, err := console.ForProvider(owning.Spec.Cluster.Provider)
		if err != nil {
			return err
		}

		// Resolve the cluster's control-plane endpoints (same source as
		// `kc console` uses) unless the user provided --endpoint.
		endpoints := machineConsoleEndpoints
		if len(endpoints) == 0 {
			resolved, src, warnings, rerr := login.ResolveControlPlaneEndpoints(
				ctx, hit.client.Ctrl, owning.Namespace, clusterID, machineConsoleUseVIP,
			)
			if rerr != nil {
				return rerr
			}
			for _, w := range warnings {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "⚠️  %s\n", w)
			}
			if len(resolved) > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "ℹ️  endpoints from %s\n", src)
			}
			endpoints = resolved
		}
		if len(endpoints) == 0 {
			return fmt.Errorf("could not resolve any control-plane endpoints for cluster %s; override with --endpoint", clusterID)
		}

		// Pick the target node IP. If the user specified --node, honour it.
		// Otherwise prefer the machine IP that intersects the CPVIP pool
		// (the cert-valid one), then fall back to privateIPAddresses, then
		// any address on the machine.
		nodes := machineConsoleNodes
		if len(nodes) == 0 {
			nodes = pickMachineNodes(hit.machine, endpoints)
		}
		if len(nodes) == 0 {
			return fmt.Errorf("machine %s has no addresses populated; specify --node <addr>", hit.machine.Name)
		}

		return handler.MachineConsole(ctx, console.MachineRequest{
			Machine:   hit.machine,
			Cluster:   owning,
			Secret:    secret,
			Client:    hit.client,
			Endpoints: endpoints,
			Nodes:     nodes,
		})
	},
}

// findMachineAcrossAZs locates a single Machine by name across all the
// provided availability zones. Errors on zero or multiple matches.
func findMachineAcrossAZs(
	ctx context.Context,
	clients []*kube.Client,
	name, namespace string,
) (*machineHit, error) {
	hits := collectMachines(ctx, clients, namespace)
	var matches []machineHit
	for _, h := range hits {
		if h.machine.Name == name {
			matches = append(matches, h)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("❌ no machine named %q found on any availability zone", name)
	case 1:
		return &matches[0], nil
	default:
		var parts []string
		for _, h := range matches {
			parts = append(parts, fmt.Sprintf("%s:%s/%s", h.azName, h.machine.Namespace, h.machine.Name))
		}
		return nil, fmt.Errorf(
			"❌ multiple machines named %q: %s — use -z/--availabilityzone (and/or -n) to disambiguate",
			name, strings.Join(parts, ", "),
		)
	}
}

// clusterIDFromMachineName extracts the clusterId prefix from a machine
// name that follows the vitistack convention.
func clusterIDFromMachineName(name string) (string, error) {
	for _, suffix := range machineNameSuffixes {
		if i := strings.LastIndex(name, suffix); i > 0 {
			// Ensure what follows the suffix is digit-only (so "my-ctp-test"
			// doesn't get misread as clusterId "my" + "-ctp" + "-test").
			tail := name[i+len(suffix):]
			if tail != "" && allDigits(tail) {
				return name[:i], nil
			}
		}
	}
	return "", fmt.Errorf("machine name %q does not match the vitistack control-plane/worker naming scheme (<clusterId>-ctp<N> or <clusterId>-wrk<N>) — can't infer owning cluster", name)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// findClusterByID finds a KubernetesCluster whose spec.data.clusterId
// matches the given id, preferring one in the same namespace as the
// machine (the operator pattern keeps cluster + machines co-located).
func findClusterByID(ctx context.Context, c *kube.Client, clusterID, preferredNs string) (*vitiv1alpha1.KubernetesCluster, error) {
	var list vitiv1alpha1.KubernetesClusterList
	opts := []ctrlclient.ListOption{}
	if preferredNs != "" {
		opts = append(opts, ctrlclient.InNamespace(preferredNs))
	}
	if err := c.Ctrl.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("listing KubernetesClusters: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.Cluster.ClusterId == clusterID {
			return &list.Items[i], nil
		}
	}
	if preferredNs != "" {
		// Retry across all namespaces in case the cluster CR lives elsewhere.
		return findClusterByID(ctx, c, clusterID, "")
	}
	return nil, fmt.Errorf("no KubernetesCluster with clusterId=%q found on this availability zone", clusterID)
}

// pickMachineNodes chooses which of the machine's addresses to pass as
// talosctl --nodes. The Talos node cert only includes a subset of the
// addresses we see in status.ipAddresses (internal/overlay networks
// often show up there but aren't in the SAN), so picking the wrong one
// causes "x509: certificate is valid for X, not Y".
//
// Strategy, in order:
//  1. Any machine IP that also appears in `endpoints` (typically the
//     CPVIP pool) — for control-plane machines this is the cert-valid IP.
//  2. status.privateIPAddresses if populated — the cluster-internal
//     addresses are typically in the cert.
//  3. status.ipAddresses as a last resort (may trigger TLS errors; user
//     can pass --node explicitly if so).
func pickMachineNodes(m *vitiv1alpha1.Machine, endpoints []string) []string {
	if m == nil {
		return nil
	}
	if inter := intersect(m.Status.IPAddresses, endpoints); len(inter) > 0 {
		return inter
	}
	if inter := intersect(m.Status.PrivateIPAddresses, endpoints); len(inter) > 0 {
		return inter
	}
	if len(m.Status.PrivateIPAddresses) > 0 {
		return m.Status.PrivateIPAddresses
	}
	if len(m.Status.IPAddresses) > 0 {
		return m.Status.IPAddresses
	}
	if len(m.Status.PublicIPAddresses) > 0 {
		return m.Status.PublicIPAddresses
	}
	return nil
}

// intersect returns the elements of a that also appear in b, preserving
// a's order and deduplicating.
func intersect(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(b))
	for _, v := range b {
		set[v] = struct{}{}
	}
	seen := make(map[string]struct{}, len(a))
	out := make([]string, 0, len(a))
	for _, v := range a {
		if _, ok := set[v]; !ok {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func init() {
	machineConsoleCmd.Flags().StringVarP(&machineConsoleNamespace, "namespace", "n", "", "namespace of the Machine")
	machineConsoleCmd.Flags().StringArrayVar(&machineConsoleEndpoints, "endpoint", nil,
		"explicit Talos API endpoint (repeatable); default: the cluster's CPVIP pool")
	machineConsoleCmd.Flags().StringArrayVar(&machineConsoleNodes, "node", nil,
		"explicit target node address (repeatable); default: the machine's IP that intersects the endpoints")
	machineConsoleCmd.Flags().BoolVar(&machineConsoleUseVIP, "use-vip", false,
		"include the CPVIP load-balancer address(es) in the endpoint list (default: control-plane node IPs only)")

	machineCmd.AddCommand(machineConsoleCmd)
}
