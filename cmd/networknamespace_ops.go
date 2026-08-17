package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

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

func init() {
	nnOrphansCmd.Flags().StringVarP(&nnOrphansNamespace, "namespace", "n", "", "limit the audit to this namespace")
	nnOrphansCmd.Flags().DurationVar(&nnOrphansZoneTimeout, "zone-timeout", 30*time.Second,
		"per-availability-zone query budget; a zone that exceeds it is reported and skipped")
	networkNamespaceCmd.AddCommand(nnOrphansCmd)
}
