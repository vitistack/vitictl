package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	nnOrphansOutput      string
)

// zoneWarn reports a non-fatal problem through the command's own stderr
// rather than the package-level warn (which writes to os.Stderr directly), so
// that coverage reporting is capturable end to end in a test.
func zoneWarn(cmd *cobra.Command) func(error) {
	return func(err error) {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "⚠️  %v\n", err)
	}
}

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

// zoneSnapshot is one zone's audit result, kept positional so results can be
// consumed in client order.
type zoneSnapshot struct {
	snap *netns.Snapshot
	err  error
}

// loadAllZones snapshots every availability zone concurrently, following
// collectClusters (cmd/kubernetescluster.go): wall clock is the slowest zone
// rather than the sum, which matters here because an unhealthy zone can only
// fail by burning its entire --zone-timeout. Each goroutine writes to its own
// slot and shares nothing; results stay in client order so output and
// coverage accounting remain deterministic.
func loadAllZones(ctx context.Context, clients []*kube.Client, namespace string, timeout time.Duration) []zoneSnapshot {
	out := make([]zoneSnapshot, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go func(i int, c *kube.Client) {
			defer wg.Done()
			snap, err := loadZoneSnapshot(ctx, c, namespace, timeout)
			out[i] = zoneSnapshot{snap: snap, err: err}
		}(i, c)
	}
	wg.Wait()
	return out
}

// auditSummary renders the coverage accounting for `nn orphans`.
//
// configured is the number of CONFIGURED availability zones — never the
// number that happened to connect. A zone whose kubeconfig is unreachable is
// dropped by ConnectAll before any query runs, so counting only the survivors
// made an audit of 2 zones out of 5 print "2/2 availability zone(s) audited"
// and suppress the incomplete-audit warning entirely: a fleet-wide clean
// verdict issued over three zones nobody looked at.
//
// A non-empty warning means the caller must fail the command: partial
// coverage has to be detectable by exit code, not only by reading stderr.
// With zero coverage there is no line at all — a clean-sweep broom over an
// audit that never happened is the worst possible output.
func auditSummary(orphans, audited, configured int) (line, warning string) {
	scope := fmt.Sprintf("%d/%d availability zone(s) audited", audited, configured)
	switch {
	case audited == 0:
		warning = fmt.Sprintf("incomplete audit: NO availability zone could be audited (0/%d) — "+
			"nothing was verified, and this is NOT a clean result", configured)
		return "", warning
	case orphans == 0:
		line = fmt.Sprintf("🧹 no orphaned networknamespaces found (%s)\n", scope)
	default:
		line = fmt.Sprintf("\n%d orphan(s), %s. Delete deliberately with: viti nn delete <name>\n", orphans, scope)
	}
	if audited < configured {
		warning = fmt.Sprintf("incomplete audit: %d of %d availability zone(s) could not be audited — "+
			"orphaned networknamespaces there are NOT listed above", configured-audited, configured)
	}
	return line, warning
}

var nnOrphansCmd = &cobra.Command{
	Use:   "orphans",
	Short: "List NetworkNamespaces nothing uses (housekeeping audit)",
	Long: `Lists every NetworkNamespace that nothing claims, across all configured
availability zones. A netns is claimed — and so not listed — when any of these
holds in its namespace:

  • a KubernetesCluster names it (spec.data.networkNamespaceName)
  • a NetworkConfiguration is bound to it, by name or by vlan<id> interface
  • an IPAllocation references it (where that CRD exists)

The NetworkConfiguration rule matters more than it looks: clusters created
before ~2026-06-02 predate the operator writing networkNamespaceName, so they
name no netns at all. Selecting on the KubernetesCluster reference alone
listed those live clusters' netns as orphans; their machines' vlan interfaces
are what proves the VLAN is still carrying traffic.

Columns:
  IPALLOCS     IPAllocations referencing it (n/a where the CRD is absent);
               a trailing "+N?" counts records whose spec.networkNamespaceName
               could not be read — unknown is not absence, so delete refuses
               on them and the netns is listed but NOT deletable
  GHOST-ASSOC  stale status association ids with no live cluster — a known
               cosmetic operator bug, shown for visibility, never trusted

Read-only. An orphan is a deletion CANDIDATE, not a verdict: an empty team
namespace may be about to receive new clusters.

Exits non-zero if any configured availability zone could not be audited, so
automation cannot mistake partial coverage for a clean fleet.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		format, err := printer.Parse(nnOrphansOutput)
		if err != nil {
			return err
		}
		zones, err := kube.ResolveAvailabilityZones(AvailabilityZone())
		if err != nil {
			return err
		}
		warnf := zoneWarn(cmd)
		// allowPartial: a fleet audit is still worth running with a zone down,
		// as long as the coverage it reports is honest about it — which is
		// what auditSummary and the non-zero exit below are for.
		clients, err := kube.ConnectAll(ctx, zones, true, warnf)
		if err != nil {
			return err
		}

		var found []orphanReport
		total, audited := 0, 0
		for i, res := range loadAllZones(ctx, clients, nnOrphansNamespace, nnOrphansZoneTimeout) {
			c := clients[i]
			if res.err != nil {
				warnf(fmt.Errorf("availability zone %q NOT audited: %w", c.AZ.Name, res.err))
				continue
			}
			audited++
			for _, o := range netns.Orphans(res.snap) {
				total++
				found = append(found, newOrphanReport(c.AZ.Name, o))
			}
		}

		// Structured output is the whole payload: no table, no summary prose —
		// callers piping this want the records, and the coverage numbers travel
		// with them so a partial audit stays detectable after the pipe.
		if format.IsStructured() {
			report := orphanAudit{Orphans: found, ZonesAudited: audited, ZonesConfigured: len(zones)}
			if report.Orphans == nil {
				report.Orphans = []orphanReport{}
			}
			if err := writeOrphanAudit(cmd.OutOrStdout(), format, report); err != nil {
				return err
			}
			if audited < len(zones) {
				_, warning := auditSummary(total, audited, len(zones))
				return errors.New(warning)
			}
			return nil
		}

		if format == printer.FormatName {
			for _, o := range found {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "networknamespace/%s/%s\n", o.Namespace, o.Name)
			}
			if audited < len(zones) {
				_, warning := auditSummary(total, audited, len(zones))
				return errors.New(warning)
			}
			return nil
		}

		// House style (cmd/resource_builder.go): no header without rows.
		if len(found) > 0 {
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			// No NC-REFS column: a NetworkConfiguration bound to the netns now
			// disqualifies it from the list entirely, so the count is zero for
			// every row printed here and the column carried no information.
			_, _ = fmt.Fprintln(tw, "AZ\tNAMESPACE\tNAME\tVLAN\tIPV4 PREFIX\tPHASE\tAGE\tIPALLOCS\tGHOST-ASSOC")
			for _, o := range found {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%d\n",
					o.AvailabilityZone, o.Namespace, o.Name, o.VlanID,
					valueOrDash(o.IPv4Prefix), o.Phase, o.Age,
					o.ipAllocCell, len(o.GhostAssocIDs))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
		}

		// Coverage is reported explicitly: a zone that could not be queried
		// must never read as a zone with nothing in it.
		line, warning := auditSummary(total, audited, len(zones))
		if line != "" {
			_, _ = fmt.Fprint(cmd.OutOrStdout(), line)
		}
		if warning != "" {
			return errors.New(warning)
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

// findNetNSAcrossAZs resolves exactly one NetworkNamespace by name: zero hits
// and ambiguous hits are both errors, never a guess. A zone that cannot be
// queried aborts the search rather than narrowing it — silently resolving
// "the only match" out of a partially-searched fleet is how the wrong object
// gets deleted.
//
// That guarantee only holds if every configured zone is represented in
// clients, so callers must connect without allowPartial (or refuse on a short
// client list themselves) before calling this. kc delete does narrow
// silently at the connect stage; this is deliberately stricter.
func findNetNSAcrossAZs(ctx context.Context, clients []*kube.Client, name, namespace string, zoneTimeout time.Duration) (*nnHit, error) {
	lists := make([]*vitiv1alpha1.NetworkNamespaceList, len(clients))
	errs := make([]error, len(clients))
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go func(i int, c *kube.Client) {
			defer wg.Done()
			lists[i], errs[i] = listZoneNetNS(ctx, c, namespace, zoneTimeout)
		}(i, c)
	}
	wg.Wait()

	var matches []nnHit
	for i, c := range clients {
		if errs[i] != nil {
			return nil, fmt.Errorf("availability zone %q could not be searched, so %q cannot be resolved unambiguously: %w",
				c.AZ.Name, name, errs[i])
		}
		for j := range lists[i].Items {
			if lists[i].Items[j].Name == name {
				matches = append(matches, nnHit{client: c, nn: &lists[i].Items[j]})
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
// cmd.InOrStdin so it is testable and scriptable. Input that ends without a
// newline is refused rather than accepted: for a teardown of external state,
// a truncated answer is not consent (use --yes to script it).
func confirmTypedName(cmd *cobra.Command, subject, action, name string) error {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Type the %s name to confirm %s: ", subject, action)
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
  • IPAllocations referencing it remain (where that CRD exists), including any
    whose spec.networkNamespaceName could not be read — unknown is not absence

Every configured availability zone must be reachable: a same-named
networknamespace on a zone that could not be searched would make the target
ambiguous, and ambiguity is never resolved by guessing. The gates are
re-checked after confirmation, so a cluster created while the prompt was open
is not deleted around.

The stale status summary (associatedKubernetesClusterIds) is shown but never
trusted. Finalizers are never stripped: if teardown hangs, investigate the
networknamespace operator on the management cluster.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		ctx := context.Background()
		out := cmd.OutOrStdout()

		zones, err := kube.ResolveAvailabilityZones(AvailabilityZone())
		if err != nil {
			return err
		}
		clients, err := kube.ConnectAll(ctx, zones, true, zoneWarn(cmd))
		if err != nil {
			return err
		}
		if err := requireWholeFleet(clients, zones, "networknamespace", name); err != nil {
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

		_, _ = fmt.Fprintf(out, "\nAbout to DELETE networknamespace:\n")
		_, _ = fmt.Fprintf(out, "  availability zone : %s\n", hit.client.AZ.Name)
		_, _ = fmt.Fprintf(out, "  namespace         : %s\n", hit.nn.Namespace)
		_, _ = fmt.Fprintf(out, "  name              : %s (phase: %s, age: %s)\n",
			hit.nn.Name, hit.nn.Status.Phase, printer.Age(hit.nn.CreationTimestamp))
		_, _ = fmt.Fprintf(out, "  external NAM state: VLAN %d, %s / %s, egress %s\n",
			hit.nn.Status.VlanID, valueOrDash(hit.nn.Status.IPv4Prefix),
			valueOrDash(hit.nn.Status.IPv6Prefix), valueOrDash(hit.nn.Status.IPv4EgressIP))

		if ev.Blocked() {
			printBlockingGates(cmd, hit.nn.Namespace, &ev)
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
			if err := confirmTypedName(cmd, "networknamespace", "deletion", name); err != nil {
				return err
			}
		}

		// The evidence above was gathered before a prompt that may have been
		// open for hours. Re-check against fresh state: a KubernetesCluster
		// created against this netns in the meantime must block, not be
		// deleted around.
		target, err := recheckGates(ctx, cmd, hit)
		if err != nil {
			return err
		}

		opts := []ctrlclient.DeleteOption{}
		if uid := target.UID; uid != "" {
			// Delete exactly the object the gates were evaluated against, not
			// whatever now answers to that name.
			opts = append(opts, ctrlclient.Preconditions{UID: &uid})
		}
		return netns.DeleteAndWait(ctx, hit.client.Ctrl, target, nnDeleteTimeout, out, opts...)
	},
}

// printBlockingGates prints every hard gate that refuses the deletion.
func printBlockingGates(cmd *cobra.Command, namespace string, ev *netns.Evidence) {
	out := cmd.OutOrStdout()
	for _, k := range ev.ReferencingKCs {
		_, _ = fmt.Fprintf(out, "  ❌ KubernetesCluster still references it: %s/%s\n", namespace, k)
	}
	for _, n := range ev.NCRefs {
		_, _ = fmt.Fprintf(out, "  ❌ NetworkConfiguration still bound: %s/%s\n", namespace, n)
	}
	if ev.IPAllocCount > 0 {
		_, _ = fmt.Fprintf(out, "  ❌ %d IPAllocation(s) still reference it\n", ev.IPAllocCount)
	}
	if n := len(ev.IPAllocUnevaluated); n > 0 {
		_, _ = fmt.Fprintf(out, "  ❌ %d IPAllocation(s) could not be evaluated (no readable spec.networkNamespaceName): %s\n",
			n, strings.Join(ev.IPAllocUnevaluated, ", "))
		_, _ = fmt.Fprintf(out, "     inspect them with: kubectl -n %s get ipallocations.vitistack.io -o yaml\n", namespace)
	}
}

// recheckGates re-reads the zone and re-evaluates every hard gate immediately
// before the delete, returning the fresh object to act on. Nothing is deleted
// on a failed or inconclusive re-check.
func recheckGates(ctx context.Context, cmd *cobra.Command, hit *nnHit) (*vitiv1alpha1.NetworkNamespace, error) {
	snap, err := loadZoneSnapshot(ctx, hit.client, hit.nn.Namespace, nnDeleteZoneTimeout)
	if err != nil {
		return nil, fmt.Errorf("re-checking the gates before deleting (nothing deleted): %w", err)
	}
	var fresh *vitiv1alpha1.NetworkNamespace
	for i := range snap.NetNSs {
		if snap.NetNSs[i].Name == hit.nn.Name {
			fresh = &snap.NetNSs[i]
			break
		}
	}
	if fresh == nil {
		return nil, fmt.Errorf("networknamespace %s/%s is no longer there — it was deleted by someone else while this command was waiting; nothing done",
			hit.nn.Namespace, hit.nn.Name)
	}
	ev := netns.EvidenceFor(snap, fresh)
	if ev.Blocked() {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\n⚠️  state changed since the gates were checked:\n")
		printBlockingGates(cmd, fresh.Namespace, &ev)
		return nil, fmt.Errorf("refusing to delete %s: it became in use while this command was waiting (gates above); nothing deleted", fresh.Name)
	}
	return fresh, nil
}

func init() {
	nnOrphansCmd.Flags().StringVarP(&nnOrphansNamespace, "namespace", "n", "", "limit the audit to this namespace")
	nnOrphansCmd.Flags().StringVarP(&nnOrphansOutput, "output", "o", "", outputFlagHelp)
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
