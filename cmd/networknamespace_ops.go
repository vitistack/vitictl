package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/netns"
	"github.com/vitistack/vitictl/internal/printer"
)

var (
	nnOrphansNamespace   string
	nnOrphansZoneTimeout time.Duration
)

// loadZoneSnapshot bounds one availability zone's queries so a single
// unhealthy zone cannot stall a fleet-wide audit. This is not theoretical:
// a zone whose CRD conversion webhook is broken makes the API server hang
// on every request for the version this client asks for, with no error and
// no timeout of its own.
func loadZoneSnapshot(ctx context.Context, c *kube.Client, namespace string, timeout time.Duration) (*netns.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	snap, err := netns.Load(ctx, c.Ctrl, namespace)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("timed out after %s — the zone's API did not answer; "+
			"check the vitistack CRD conversion webhook on this cluster: %w", timeout, err)
	}
	return snap, err
}

var nnOrphansCmd = &cobra.Command{
	Use:   "orphans",
	Short: "List NetworkNamespaces no KubernetesCluster references (housekeeping audit)",
	Long: `Lists every NetworkNamespace with zero referencing KubernetesClusters
(spec.data.networkNamespaceName), across all configured availability zones.

Columns preview the 'viti nn delete' gates:
  NC-REFS      NetworkConfigurations still bound to the netns (by name or vlan)
  IPALLOCS     IPAllocations still referencing it (n/a where the CRD is absent)
  GHOST-ASSOC  stale status association ids with no live cluster — a known
               cosmetic operator bug, shown for visibility, never trusted

Read-only. An orphan is a deletion CANDIDATE, not a verdict: an empty team
namespace may be about to receive new clusters.`,
	Args: cobra.NoArgs,
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

		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "AZ\tNAMESPACE\tNAME\tVLAN\tIPV4 PREFIX\tPHASE\tAGE\tNC-REFS\tIPALLOCS\tGHOST-ASSOC")
		total := 0
		audited := len(clients)
		for _, c := range clients {
			snap, err := loadZoneSnapshot(ctx, c, nnOrphansNamespace, nnOrphansZoneTimeout)
			if err != nil {
				warn(fmt.Errorf("availability zone %q NOT audited: %w", c.AZ.Name, err))
				audited--
				continue
			}
			for _, o := range netns.Orphans(snap) {
				total++
				ipallocs := "n/a"
				if o.Ev.IPAllocCount >= 0 {
					ipallocs = strconv.Itoa(o.Ev.IPAllocCount)
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%d\t%s\t%d\n",
					c.AZ.Name, o.NN.Namespace, o.NN.Name,
					o.NN.Status.VlanID, valueOrDash(o.NN.Status.IPv4Prefix), o.NN.Status.Phase,
					printer.Age(o.NN.CreationTimestamp),
					len(o.Ev.NCRefs), ipallocs, len(o.Ev.GhostAssocIDs))
			}
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		// Coverage is reported explicitly: a zone that could not be queried
		// must never read as a zone with nothing in it.
		scope := fmt.Sprintf("%d/%d availability zone(s) audited", audited, len(clients))
		if total == 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "🧹 no orphaned networknamespaces found (%s)\n", scope)
		} else {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\n%d orphan(s), %s. Delete deliberately with: viti nn delete <name>\n", total, scope)
		}
		if audited < len(clients) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"⚠️  incomplete audit: %d zone(s) were skipped — orphans there are NOT listed above\n", len(clients)-audited)
		}
		return nil
	},
}

var (
	nnDeleteNamespace   string
	nnDeleteYes         bool
	nnDeleteTimeout     time.Duration
	nnDeleteZoneTimeout time.Duration
)

type nnHit struct {
	client *kube.Client
	nn     *vitiv1alpha1.NetworkNamespace
}

// findNetNSAcrossAZs resolves exactly one NetworkNamespace by name — the same
// exactly-one discipline as kc delete: zero hits and ambiguous hits are both
// errors, never a guess. A zone that cannot be queried aborts the search
// rather than narrowing it: silently resolving "the only match" out of a
// partially-searched fleet is how the wrong object gets deleted.
func findNetNSAcrossAZs(ctx context.Context, clients []*kube.Client, name, namespace string, zoneTimeout time.Duration) (*nnHit, error) {
	var matches []nnHit
	for _, c := range clients {
		l, err := listZoneNetNS(ctx, c, namespace, zoneTimeout)
		if err != nil {
			return nil, fmt.Errorf("availability zone %q could not be searched, so %q cannot be resolved unambiguously: %w",
				c.AZ.Name, name, err)
		}
		for i := range l.Items {
			if l.Items[i].Name == name {
				matches = append(matches, nnHit{client: c, nn: &l.Items[i]})
			}
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("❌ no networknamespace named %q found on any availability zone", name)
	case 1:
		return &matches[0], nil
	default:
		where := make([]string, 0, len(matches))
		for _, m := range matches {
			where = append(where, fmt.Sprintf("%s/%s", m.client.AZ.Name, m.nn.Namespace))
		}
		return nil, fmt.Errorf("❌ networknamespace %q is ambiguous (found in: %s) — narrow with -z and/or -n",
			name, strings.Join(where, ", "))
	}
}

func listZoneNetNS(ctx context.Context, c *kube.Client, namespace string, timeout time.Duration) (*vitiv1alpha1.NetworkNamespaceList, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var l vitiv1alpha1.NetworkNamespaceList
	var opts []ctrlclient.ListOption
	if namespace != "" {
		opts = append(opts, ctrlclient.InNamespace(namespace))
	}
	if err := c.Ctrl.List(ctx, &l, opts...); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("timed out after %s — check the vitistack CRD conversion webhook on this cluster: %w", timeout, err)
		}
		return nil, fmt.Errorf("listing networknamespaces: %w", err)
	}
	return &l, nil
}

// confirmTypedName requires the operator to type the resource name back —
// a stronger guard than yes/no for an irreversible operation. Reads from
// cmd.InOrStdin so it is testable and scriptable.
func confirmTypedName(cmd *cobra.Command, what, name string) error {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Type the %s name to confirm deletion: ", what)
	r := bufio.NewReader(cmd.InOrStdin())
	answer, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	if strings.TrimSpace(answer) != name {
		return fmt.Errorf("aborted: confirmation did not match")
	}
	return nil
}

var nnDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete an UNUSED NetworkNamespace (gated, DESTRUCTIVE)",
	Long: `Deletes one NetworkNamespace after verifying nothing uses it, then waits
for the operator to release the finalizer — which is what tears down the
external NAM state (VLAN, IPv4/IPv6 prefixes, egress IP).

Hard gates — ANY of these refuses the deletion (no override):
  • a KubernetesCluster in the namespace references it (spec.data.networkNamespaceName)
  • a NetworkConfiguration is bound to it (by name or vlan<id> interface)
  • IPAllocations referencing it remain (where that CRD exists)

The stale status summary (associatedKubernetesClusterIds) is shown but never
trusted. Finalizers are never stripped: if teardown hangs, investigate the
networknamespace operator on the management cluster.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		ctx := context.Background()

		zones, err := kube.ResolveAvailabilityZones(AvailabilityZone())
		if err != nil {
			return err
		}
		clients, err := kube.ConnectAll(ctx, zones, true, warn)
		if err != nil {
			return err
		}
		hit, err := findNetNSAcrossAZs(ctx, clients, name, nnDeleteNamespace, nnDeleteZoneTimeout)
		if err != nil {
			return err
		}

		snap, err := loadZoneSnapshot(ctx, hit.client, hit.nn.Namespace, nnDeleteZoneTimeout)
		if err != nil {
			return fmt.Errorf("gathering evidence (nothing deleted): %w", err)
		}
		ev := netns.EvidenceFor(snap, hit.nn)

		out := cmd.OutOrStdout()
		_, _ = fmt.Fprintf(out, "\nAbout to DELETE networknamespace:\n")
		_, _ = fmt.Fprintf(out, "  availability zone : %s\n", hit.client.AZ.Name)
		_, _ = fmt.Fprintf(out, "  namespace         : %s\n", hit.nn.Namespace)
		_, _ = fmt.Fprintf(out, "  name              : %s (phase: %s, age: %s)\n",
			hit.nn.Name, hit.nn.Status.Phase, printer.Age(hit.nn.CreationTimestamp))
		_, _ = fmt.Fprintf(out, "  external NAM state: VLAN %d, %s / %s, egress %s\n",
			hit.nn.Status.VlanID, valueOrDash(hit.nn.Status.IPv4Prefix),
			valueOrDash(hit.nn.Status.IPv6Prefix), valueOrDash(hit.nn.Status.IPv4EgressIP))

		if ev.Blocked() {
			for _, k := range ev.ReferencingKCs {
				_, _ = fmt.Fprintf(out, "  ❌ KubernetesCluster still references it: %s/%s\n", hit.nn.Namespace, k)
			}
			for _, n := range ev.NCRefs {
				_, _ = fmt.Fprintf(out, "  ❌ NetworkConfiguration still bound: %s/%s\n", hit.nn.Namespace, n)
			}
			if ev.IPAllocCount > 0 {
				_, _ = fmt.Fprintf(out, "  ❌ %d IPAllocation(s) still reference it\n", ev.IPAllocCount)
			}
			return fmt.Errorf("refusing to delete %s: it is in use (gates above)", name)
		}

		_, _ = fmt.Fprintf(out, "  ✅ no KubernetesCluster references it\n")
		_, _ = fmt.Fprintf(out, "  ✅ no NetworkConfiguration bound to it\n")
		switch {
		case ev.IPAllocCount == 0:
			_, _ = fmt.Fprintf(out, "  ✅ no IPAllocations reference it\n")
		case ev.IPAllocCount < 0:
			_, _ = fmt.Fprintf(out, "  ⚠️  IPAllocation CRD not present on this zone — that gate could not run\n")
		}
		if !ev.VlanKnown {
			_, _ = fmt.Fprintf(out, "  ⚠️  no VLAN assigned (never provisioned?) — vlan-interface gate not applicable\n")
		}
		for _, g := range ev.GhostAssocIDs {
			_, _ = fmt.Fprintf(out, "  ℹ️  stale status association (known operator bug, ignored): %s\n", g)
		}
		_, _ = fmt.Fprintf(out, "\nThis releases the VLAN and prefixes in NAM and is IRREVERSIBLE.\n")

		if !nnDeleteYes {
			if err := confirmTypedName(cmd, "networknamespace", name); err != nil {
				return err
			}
		}
		return netns.DeleteAndWait(ctx, hit.client.Ctrl, hit.nn, nnDeleteTimeout, out)
	},
}

func init() {
	nnOrphansCmd.Flags().StringVarP(&nnOrphansNamespace, "namespace", "n", "", "limit the audit to this namespace")
	nnOrphansCmd.Flags().DurationVar(&nnOrphansZoneTimeout, "zone-timeout", 30*time.Second,
		"per-availability-zone query budget; a zone that exceeds it is reported and skipped")

	nnDeleteCmd.Flags().StringVarP(&nnDeleteNamespace, "namespace", "n", "", "namespace of the networknamespace")
	nnDeleteCmd.Flags().BoolVar(&nnDeleteYes, "yes", false, "skip the confirmation prompt")
	nnDeleteCmd.Flags().DurationVar(&nnDeleteTimeout, "timeout", 2*time.Minute,
		"how long to wait for the operator to release the finalizer")
	nnDeleteCmd.Flags().DurationVar(&nnDeleteZoneTimeout, "zone-timeout", 30*time.Second,
		"per-availability-zone query budget while resolving the target")

	networkNamespaceCmd.AddCommand(nnOrphansCmd, nnDeleteCmd)
}
