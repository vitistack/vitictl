package cmd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/extract"
	"github.com/vitistack/vitictl/internal/fuzzy"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/printer"
	"github.com/vitistack/vitictl/internal/talos"
)

var kubernetesClusterCmd = &cobra.Command{
	Use:     "kubernetescluster",
	Aliases: []string{"kc", "kubernetesclusters"},
	Short:   "☸️  Work with KubernetesCluster resources",
}

var (
	kcListNamespace   string
	kcListOutput      string
	kcListSort        string
	kcGetNamespace    string
	kcGetOutput       string
	kcSearchNamespace string
	kcSearchOutput    string
	kcSearchSort      string
	kcConfigNamespace string
	kcConfigOutputDir string
)

type kcHit struct {
	client  *kube.Client
	cluster *vitiv1alpha1.KubernetesCluster
}

// kcComparators returns the sort comparators supported by KubernetesCluster
// list/search. Defaults: az, namespace, name, age. Plus the values shown in
// the table columns (cluster-id, provider, phase, region, env, datacenter,
// version, cp-replicas).
func kcComparators() map[string]func(a, b kcHit) int {
	return map[string]func(a, b kcHit) int{
		"az":        func(a, b kcHit) int { return cmpStrings(a.client.AZ.Name, b.client.AZ.Name) },
		"namespace": func(a, b kcHit) int { return cmpStrings(a.cluster.Namespace, b.cluster.Namespace) },
		"name":      func(a, b kcHit) int { return cmpStrings(a.cluster.Name, b.cluster.Name) },
		"cluster-id": func(a, b kcHit) int {
			return cmpStrings(a.cluster.Spec.Cluster.ClusterId, b.cluster.Spec.Cluster.ClusterId)
		},
		"provider": func(a, b kcHit) int {
			return cmpStrings(string(a.cluster.Spec.Cluster.Provider), string(b.cluster.Spec.Cluster.Provider))
		},
		"phase":  func(a, b kcHit) int { return cmpStrings(a.cluster.Status.Phase, b.cluster.Status.Phase) },
		"region": func(a, b kcHit) int { return cmpStrings(a.cluster.Spec.Cluster.Region, b.cluster.Spec.Cluster.Region) },
		"env": func(a, b kcHit) int {
			return cmpStrings(a.cluster.Spec.Cluster.Environment, b.cluster.Spec.Cluster.Environment)
		},
		"datacenter": func(a, b kcHit) int {
			return cmpStrings(a.cluster.Spec.Cluster.Datacenter, b.cluster.Spec.Cluster.Datacenter)
		},
		"version": func(a, b kcHit) int {
			return cmpStrings(a.cluster.Spec.Topology.Version, b.cluster.Spec.Topology.Version)
		},
		"cp-replicas": func(a, b kcHit) int {
			ra, rb := a.cluster.Spec.Topology.ControlPlane.Replicas, b.cluster.Spec.Topology.ControlPlane.Replicas
			switch {
			case ra < rb:
				return -1
			case ra > rb:
				return 1
			}
			return 0
		},
		"age": func(a, b kcHit) int {
			ta := a.cluster.CreationTimestamp.Time
			tb := b.cluster.CreationTimestamp.Time
			if ta.Equal(tb) {
				return 0
			}
			if ta.After(tb) {
				return -1
			}
			return 1
		},
	}
}

func collectClusters(
	ctx context.Context,
	clients []*kube.Client,
	namespace string,
) []kcHit {
	// Query all availability zones concurrently — wall clock is the slowest
	// zone, not the sum. Results keep the original client order so output
	// stays deterministic; errors are surfaced after the fan-in (warn is not
	// assumed goroutine-safe).
	perClient := make([][]kcHit, len(clients))
	errs := make([]error, len(clients))
	var wg sync.WaitGroup
	for idx, c := range clients {
		wg.Add(1)
		go func(idx int, c *kube.Client) {
			defer wg.Done()
			var list vitiv1alpha1.KubernetesClusterList
			opts := []ctrlclient.ListOption{}
			if namespace != "" {
				opts = append(opts, ctrlclient.InNamespace(namespace))
			}
			if err := c.Ctrl.List(ctx, &list, opts...); err != nil {
				errs[idx] = fmt.Errorf("availability zone %q: listing kubernetesclusters: %w", c.AZ.Name, err)
				return
			}
			hits := make([]kcHit, 0, len(list.Items))
			for i := range list.Items {
				hits = append(hits, kcHit{client: c, cluster: &list.Items[i]})
			}
			perClient[idx] = hits
		}(idx, c)
	}
	wg.Wait()

	var hits []kcHit
	for idx := range clients {
		if errs[idx] != nil {
			warn(errs[idx])
			continue
		}
		hits = append(hits, perClient[idx]...)
	}
	return hits
}

var kcListCmd = &cobra.Command{
	Use:   "list",
	Short: "List KubernetesClusters across all configured availability zones",
	RunE: func(cmd *cobra.Command, args []string) error {
		format, err := printer.Parse(kcListOutput)
		if err != nil {
			return err
		}
		ctx := context.Background()
		zones, err := kube.ResolveAvailabilityZones(AvailabilityZone())
		if err != nil {
			return err
		}
		clients, err := kube.ConnectAll(ctx, zones, true, warn)
		if err != nil {
			return err
		}
		hits := collectClusters(ctx, clients, kcListNamespace)
		if err := sortByKeys(hits, kcListSort, kcComparators()); err != nil {
			return err
		}
		if len(hits) == 0 && !format.IsStructured() {
			fmt.Println("🤷 no kubernetesclusters found")
			return nil
		}
		return renderClusters(cmd, hits, format)
	},
}

var kcGetCmd = &cobra.Command{
	Use:   "get <name>",
	Short: "Show details of a KubernetesCluster (searches all availability zones unless --az is given)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		format, err := printer.Parse(kcGetOutput)
		if err != nil {
			return err
		}
		ctx := context.Background()
		zones, err := kube.ResolveAvailabilityZones(AvailabilityZone())
		if err != nil {
			return err
		}
		clients, err := kube.ConnectAll(ctx, zones, true, warn)
		if err != nil {
			return err
		}
		all := collectClusters(ctx, clients, kcGetNamespace)
		var hits []kcHit
		for _, h := range all {
			if h.cluster.Name == args[0] {
				hits = append(hits, h)
			}
		}
		if len(hits) == 0 {
			return fmt.Errorf("❌ no kubernetescluster named %q found on any availability zone", args[0])
		}
		if format == printer.FormatTable {
			for i, h := range hits {
				if i > 0 {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), strings.Repeat("-", 60))
				}
				printKubernetesCluster(cmd, h.client.AZ.Name, h.cluster)
			}
			if len(hits) > 1 {
				fmt.Printf("\n🔎 %q found on %d availability zones / namespaces\n", args[0], len(hits))
			}
			return nil
		}
		return renderClusters(cmd, hits, format)
	},
}

var kcSearchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Fuzzy-search KubernetesClusters across all configured availability zones",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		format, err := printer.Parse(kcSearchOutput)
		if err != nil {
			return err
		}
		ctx := context.Background()
		zones, err := kube.ResolveAvailabilityZones(AvailabilityZone())
		if err != nil {
			return err
		}
		clients, err := kube.ConnectAll(ctx, zones, true, warn)
		if err != nil {
			return err
		}
		all := collectClusters(ctx, clients, kcSearchNamespace)

		query := ""
		if len(args) == 1 {
			query = args[0]
		}
		candidates := make([]fuzzy.Candidate[kcHit], 0, len(all))
		for _, h := range all {
			it := h.cluster
			label := strings.Join([]string{
				h.client.AZ.Name, it.Namespace, it.Name,
				it.Spec.Cluster.ClusterId, string(it.Spec.Cluster.Provider),
				it.Spec.Cluster.Environment, it.Spec.Cluster.Region,
			}, " ")
			candidates = append(candidates, fuzzy.Candidate[kcHit]{Label: label, Item: h})
		}
		matches := fuzzy.Search(query, candidates)
		hits := make([]kcHit, 0, len(matches))
		for _, m := range matches {
			hits = append(hits, m.Item)
		}
		if err := sortByKeys(hits, kcSearchSort, kcComparators()); err != nil {
			return err
		}
		if len(hits) == 0 && !format.IsStructured() {
			fmt.Println("🤷 no kubernetesclusters matched")
			return nil
		}
		return renderClusters(cmd, hits, format)
	},
}

var kcGetConfigCmd = &cobra.Command{
	Use:     "get-config <name>",
	Aliases: []string{"getconfig", "config"},
	Short:   "Extract kubeconfig (and Talos config files) for a KubernetesCluster",
	Long: `Extract the cluster configuration stored in the KubernetesCluster's
provider secret. Searches all configured availability zones unless
-z/--availabilityzone (or --az) is given.

For AKS clusters this writes the "kubeconfig" file and an "info.txt" with the
remaining secret metadata.

For Talos clusters this writes "worker.yaml", "controlplane.yaml",
"secret.yaml" (from secrets.bundle), "talosconfig", "kubeconfig" (from
kube.config), and an "info.txt" with the remaining secret keys.

Output goes to ./<clusterId>/ by default; override with -o/--output.

Note: "get-config" uses -o for the output directory, unlike list/get/search
which use -o for format.`,
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

		hit, err := findClusterAcrossAZs(ctx, clients, args[0], kcConfigNamespace)
		if err != nil {
			return err
		}

		secret, err := extract.FindClusterSecret(ctx, hit.client.Ctrl, hit.cluster)
		if err != nil {
			return err
		}

		outDir := kcConfigOutputDir
		if outDir == "" {
			outDir = hit.cluster.Spec.Cluster.ClusterId
		}

		var summary *extract.WriteSummary
		switch hit.cluster.Spec.Cluster.Provider {
		case vitiv1alpha1.KubernetesProviderTypeTalos:
			summary, err = extract.WriteTalos(outDir, secret)
		case vitiv1alpha1.KubernetesProviderTypeAKS:
			summary, err = extract.WriteAKS(outDir, secret)
		default:
			return fmt.Errorf("unsupported provider %q for cluster %s/%s",
				hit.cluster.Spec.Cluster.Provider, hit.cluster.Namespace, hit.cluster.Name)
		}
		if err != nil {
			return err
		}

		fmt.Printf("🎯 AZ: %s\n📂 Wrote %d file(s) to %s:\n",
			hit.client.AZ.Name, len(summary.Files), summary.OutputDir)
		for _, f := range summary.Files {
			fmt.Printf("  📄 %s\n", f)
		}
		return nil
	},
}

// findClusterAcrossAZs locates a single KubernetesCluster by name across all
// provided availability zones. Errors if zero matches or ambiguous.
func findClusterAcrossAZs(
	ctx context.Context,
	clients []*kube.Client,
	name, namespace string,
) (*kcHit, error) {
	hits := collectClusters(ctx, clients, namespace)
	var matches []kcHit
	for _, h := range hits {
		if h.cluster.Name == name {
			matches = append(matches, h)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("❌ no kubernetescluster named %q found on any availability zone", name)
	case 1:
		return &matches[0], nil
	default:
		var parts []string
		for _, h := range matches {
			parts = append(parts, fmt.Sprintf("%s:%s/%s", h.client.AZ.Name, h.cluster.Namespace, h.cluster.Name))
		}
		return nil, fmt.Errorf(
			"❌ multiple kubernetesclusters named %q: %s — use -z/--availabilityzone (and/or -n) to disambiguate",
			name, strings.Join(parts, ", "),
		)
	}
}

func renderClusters(cmd *cobra.Command, hits []kcHit, format printer.Format) error {
	switch format {
	case printer.FormatJSON, printer.FormatYAML:
		objs := make([]runtime.Object, 0, len(hits))
		for _, h := range hits {
			objs = append(objs, h.cluster)
		}
		if format == printer.FormatJSON {
			return printer.WriteJSON(cmd.OutOrStdout(), objs)
		}
		return printer.WriteYAML(cmd.OutOrStdout(), objs)
	case printer.FormatName:
		for _, h := range hits {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "kubernetescluster/"+h.cluster.Namespace+"/"+h.cluster.Name)
		}
		return nil
	case printer.FormatWide:
		return writeClusterTable(cmd, hits, true)
	default:
		return writeClusterTable(cmd, hits, false)
	}
}

func writeClusterTable(cmd *cobra.Command, hits []kcHit, wide bool) error {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	var talosVersions map[string]string
	if wide {
		talosVersions = collectDeclaredTalosVersions(cmd.Context(), hits)
		_, _ = fmt.Fprintln(tw, "AZ\tNAMESPACE\tNAME\tCLUSTER ID\tPROVIDER\tPHASE\tREGION\tENV\tVERSION\tTALOS\tCP REPLICAS\tDATACENTER\tAGE")
	} else {
		_, _ = fmt.Fprintln(tw, "AZ\tNAMESPACE\tNAME\tCLUSTER ID\tPROVIDER\tPHASE\tREGION\tENV")
	}
	for _, h := range hits {
		it := h.cluster
		if wide {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				h.client.AZ.Name, it.Namespace, it.Name,
				it.Spec.Cluster.ClusterId, string(it.Spec.Cluster.Provider),
				it.Status.Phase, it.Spec.Cluster.Region, it.Spec.Cluster.Environment,
				valueOrDash(it.Spec.Topology.Version),
				valueOrDash(talosVersions[h.client.AZ.Name+"/"+it.Namespace+"/"+it.Spec.Cluster.ClusterId]),
				it.Spec.Topology.ControlPlane.Replicas,
				valueOrDash(it.Spec.Cluster.Datacenter),
				printer.Age(it.CreationTimestamp),
			)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				h.client.AZ.Name, it.Namespace, it.Name,
				it.Spec.Cluster.ClusterId, string(it.Spec.Cluster.Provider),
				it.Status.Phase, it.Spec.Cluster.Region, it.Spec.Cluster.Environment,
			)
		}
	}
	return tw.Flush()
}

// collectDeclaredTalosVersions returns the DECLARED Talos version per cluster
// (key: az/namespace/clusterId), parsed from the machines' spec.os.imageID —
// the field the operator drives provisioning and upgrades from.
//
// Cost: exactly ONE all-namespaces machine List per availability zone, all
// zones queried in parallel with a bounded timeout — the column adds roughly
// one round-trip of wall clock, not one per namespace. Best-effort: an
// unreachable zone just leaves the column blank for its clusters.
//
// Note this is desired state, not runtime truth — "viti kc health" reports
// the runtime version from the guest's kubelets and flags drift.
func collectDeclaredTalosVersions(ctx context.Context, hits []kcHit) map[string]string {
	// Unique clients (one per AZ present in the hits).
	clientsByAZ := map[string]*kube.Client{}
	for _, h := range hits {
		clientsByAZ[h.client.AZ.Name] = h.client
	}

	var (
		mu           sync.Mutex
		machinesByAZ = make(map[string][]vitiv1alpha1.Machine, len(clientsByAZ))
		wg           sync.WaitGroup
	)
	for az, c := range clientsByAZ {
		wg.Add(1)
		go func(az string, c *kube.Client) {
			defer wg.Done()
			lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			var ml vitiv1alpha1.MachineList
			if err := c.Ctrl.List(lctx, &ml); err != nil {
				return // column stays blank for this zone
			}
			mu.Lock()
			machinesByAZ[az] = ml.Items
			mu.Unlock()
		}(az, c)
	}
	wg.Wait()

	out := make(map[string]string, len(hits))
	for _, h := range hits {
		if h.cluster.Spec.Cluster.Provider != vitiv1alpha1.KubernetesProviderTypeTalos {
			continue
		}
		clusterID := h.cluster.Spec.Cluster.ClusterId
		versions := map[string]struct{}{}
		for i := range machinesByAZ[h.client.AZ.Name] {
			m := &machinesByAZ[h.client.AZ.Name][i]
			if m.Namespace != h.cluster.Namespace || !strings.HasPrefix(m.Name, clusterID+"-") {
				continue
			}
			if v := talos.VersionFromImageID(m.Spec.OS.ImageID); v != "" {
				versions[v] = struct{}{}
			}
		}
		out[h.client.AZ.Name+"/"+h.cluster.Namespace+"/"+clusterID] = talos.JoinVersions(versions)
	}
	return out
}

func printKubernetesCluster(cmd *cobra.Command, azName string, kc *vitiv1alpha1.KubernetesCluster) {
	out := cmd.OutOrStdout()
	pf := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }
	pl := func(s string) { _, _ = fmt.Fprintln(out, s) }

	pf("🎯 AZ:            %s\n", azName)
	pf("🏷️  Name:          %s\n", kc.Name)
	pf("📦 Namespace:     %s\n", kc.Namespace)
	pf("🆔 Cluster ID:    %s\n", kc.Spec.Cluster.ClusterId)
	if kc.Spec.Cluster.ClusterUID != "" {
		pf("🔖 Cluster UID:   %s\n", kc.Spec.Cluster.ClusterUID)
	}
	pf("☁️  Provider:      %s\n", string(kc.Spec.Cluster.Provider))
	pf("🌍 Region:        %s\n", kc.Spec.Cluster.Region)
	pf("📍 Zone:          %s\n", kc.Spec.Cluster.Zone)
	pf("🏷️  Environment:   %s\n", kc.Spec.Cluster.Environment)
	if kc.Spec.Cluster.Datacenter != "" {
		pf("🏢 Datacenter:    %s\n", kc.Spec.Cluster.Datacenter)
	}
	pf("🚦 Phase:         %s %s\n", phaseEmoji(kc.Status.Phase), kc.Status.Phase)
	pf("⏱️  Age:           %s\n", printer.Age(kc.CreationTimestamp))

	pl("\n🧠 Control Plane:")
	pf("  - replicas:      %d\n", kc.Spec.Topology.ControlPlane.Replicas)
	if kc.Spec.Topology.ControlPlane.Version != "" {
		pf("  - version:       %s\n", kc.Spec.Topology.ControlPlane.Version)
	}
	if kc.Spec.Topology.ControlPlane.MachineClass != "" {
		pf("  - machineClass:  %s\n", kc.Spec.Topology.ControlPlane.MachineClass)
	}

	if len(kc.Spec.Topology.Workers.NodePools) > 0 {
		pl("\n👷 Worker Node Pools:")
		for _, np := range kc.Spec.Topology.Workers.NodePools {
			pf("  - %s: replicas=%d machineClass=%s provider=%s\n",
				valueOrDash(np.Name), np.Replicas, valueOrDash(np.MachineClass), string(np.Provider))
		}
	}

	if len(kc.Status.State.Endpoints) > 0 {
		pl("\n🔗 Endpoints:")
		for _, ep := range kc.Status.State.Endpoints {
			pf("  - %-15s %s\n", ep.Name, ep.Address)
		}
	}

	if len(kc.Status.Conditions) > 0 {
		pl("\n📋 Conditions:")
		for _, c := range kc.Status.Conditions {
			pf("  - %s=%s  %s\n", c.Type, c.Status, strings.TrimSpace(c.Message))
		}
	}
}

func init() {
	kcListCmd.Flags().StringVarP(&kcListNamespace, "namespace", "n", "", "limit to this namespace")
	kcListCmd.Flags().StringVarP(&kcListOutput, "output", "o", "",
		"output format: wide, json, yaml, name (default: table)")
	kcListCmd.Flags().StringVarP(&kcListSort, "sort", "s", "", sortFlagHelpFor(kcComparators()))

	kcGetCmd.Flags().StringVarP(&kcGetNamespace, "namespace", "n", "", "namespace of the KubernetesCluster")
	kcGetCmd.Flags().StringVarP(&kcGetOutput, "output", "o", "",
		"output format: wide, json, yaml, name (default: detailed view)")

	kcSearchCmd.Flags().StringVarP(&kcSearchNamespace, "namespace", "n", "", "limit search to this namespace")
	kcSearchCmd.Flags().StringVarP(&kcSearchOutput, "output", "o", "",
		"output format: wide, json, yaml, name (default: table)")
	kcSearchCmd.Flags().StringVarP(&kcSearchSort, "sort", "s", "", sortFlagHelpFor(kcComparators()))

	kcGetConfigCmd.Flags().StringVarP(&kcConfigNamespace, "namespace", "n", "", "namespace of the KubernetesCluster")
	kcGetConfigCmd.Flags().StringVarP(&kcConfigOutputDir, "output", "o", "", "output directory (default: ./<clusterId>)")

	kubernetesClusterCmd.AddCommand(kcListCmd, kcGetCmd, kcSearchCmd, kcGetConfigCmd)
}
