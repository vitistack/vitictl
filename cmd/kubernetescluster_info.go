package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/printer"
	"github.com/vitistack/vitictl/pkg/plugin/picker"
)

// nodeResources is the hardware one node of a machine class provides, or
// the sum over a set of nodes. Memory stays a resource.Quantity so "32Gi"
// times five prints as "160Gi", not as a raw byte count, and so JSON and
// YAML carry the same string an operator would type into a MachineClass.
type nodeResources struct {
	CPU    int               `json:"cpu"`
	Memory resource.Quantity `json:"memory"`
	GPU    int               `json:"gpu,omitempty"`
}

// times returns the resources of n nodes of this size.
func (r nodeResources) times(n int) nodeResources {
	mem := r.Memory.DeepCopy()
	mem.Mul(int64(n))
	return nodeResources{CPU: r.CPU * n, Memory: mem, GPU: r.GPU * n}
}

// autoscalingInfo is the autoscaler configuration of a node pool. It is only
// present when autoscaling is enabled, so a pool without it has no key at all.
type autoscalingInfo struct {
	MinReplicas int `json:"minReplicas"`
	MaxReplicas int `json:"maxReplicas"`
}

// poolInfo describes one group of identical nodes: the control plane or a
// worker pool. PerNode and Total are absent when the machine class could not
// be resolved in the cluster's zone; the class name is still reported so the
// operator can see which one was missing.
type poolInfo struct {
	// Role is "controlplane" or "worker".
	Role         string           `json:"role"`
	Name         string           `json:"name,omitempty"`
	Replicas     int              `json:"replicas"`
	MachineClass string           `json:"machineClass,omitempty"`
	Autoscaling  *autoscalingInfo `json:"autoscaling,omitempty"`
	PerNode      *nodeResources   `json:"perNode,omitempty"`
	Total        *nodeResources   `json:"total,omitempty"`
}

const (
	roleControlPlane = "controlplane"
	roleWorker       = "worker"
)

// resourceTotals sums a set of pools. Nodes is always exact — replica counts
// come from the spec — while CPU, Memory and GPU are partial when a pool's
// machine class is unknown; Partial says so rather than letting an
// undercount pass for the truth.
type resourceTotals struct {
	Nodes   int               `json:"nodes"`
	CPU     int               `json:"cpu"`
	Memory  resource.Quantity `json:"memory"`
	GPU     int               `json:"gpu,omitempty"`
	Partial bool              `json:"partial,omitempty"`
}

func (t *resourceTotals) addPool(p poolInfo) {
	t.Nodes += p.Replicas
	if p.Total == nil {
		if p.Replicas > 0 {
			t.Partial = true
		}
		return
	}
	t.CPU += p.Total.CPU
	t.Memory.Add(p.Total.Memory)
	t.GPU += p.Total.GPU
}

// clusterTotals splits the sums by role: the control plane runs the cluster,
// the workers run the workload, and capacity planning asks about each on its
// own before it asks about the whole.
type clusterTotals struct {
	ControlPlane resourceTotals `json:"controlPlane"`
	Workers      resourceTotals `json:"workers"`
	Cluster      resourceTotals `json:"cluster"`
}

// clusterInfo is the capacity view of one KubernetesCluster: what the spec
// declares, priced in nodes, cores and memory through the machine classes of
// its zone. It is what `viti kc info` renders, in every format.
type clusterInfo struct {
	AZ                string `json:"az"`
	Namespace         string `json:"namespace"`
	Name              string `json:"name"`
	ClusterID         string `json:"clusterId,omitempty"`
	Provider          string `json:"provider,omitempty"`
	Environment       string `json:"environment,omitempty"`
	Phase             string `json:"phase,omitempty"`
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	ControlPlane poolInfo      `json:"controlPlane"`
	NodePools    []poolInfo    `json:"nodePools"`
	Totals       clusterTotals `json:"totals"`

	// UnknownMachineClasses lists the classes referenced by the spec that
	// the zone does not define. Non-empty means the totals are partial.
	UnknownMachineClasses []string `json:"unknownMachineClasses,omitempty"`
}

// machineClassIndex maps a class name to its size, for one availability zone.
type machineClassIndex map[string]nodeResources

// resourcesOf reads a MachineClass into the sizes this command reports.
func resourcesOf(mc *vitiv1alpha1.MachineClass) nodeResources {
	return nodeResources{
		CPU:    int(mc.Spec.CPU.Cores),
		Memory: mc.Spec.Memory.Quantity.DeepCopy(),
		GPU:    int(mc.Spec.GPU.Cores),
	}
}

// indexMachineClasses lists the MachineClass objects of one zone. Disabled
// classes are included on purpose: a pool created before its class was
// disabled still runs on nodes of that size.
func indexMachineClasses(ctx context.Context, c *kube.Client) (machineClassIndex, error) {
	list := &vitiv1alpha1.MachineClassList{}
	if err := c.Ctrl.List(ctx, list); err != nil {
		return nil, fmt.Errorf("availability zone %q: listing machineclasses: %w", c.AZ.Name, err)
	}
	idx := make(machineClassIndex, len(list.Items))
	for i := range list.Items {
		idx[list.Items[i].Name] = resourcesOf(&list.Items[i])
	}
	return idx, nil
}

// indexMachineClassesByAZ fetches the classes of every zone the hits span,
// once per zone and all zones in parallel. A zone whose listing fails is
// reported through warn and left out; its clusters then render with unknown
// classes rather than aborting the whole report.
func indexMachineClassesByAZ(ctx context.Context, hits []kcHit, warn func(error)) map[string]machineClassIndex {
	clientsByAZ := map[string]*kube.Client{}
	for _, h := range hits {
		clientsByAZ[h.client.AZ.Name] = h.client
	}
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		out  = make(map[string]machineClassIndex, len(clientsByAZ))
		errs []error
	)
	for az, c := range clientsByAZ {
		wg.Add(1)
		go func(az string, c *kube.Client) {
			defer wg.Done()
			idx, err := indexMachineClasses(ctx, c)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			out[az] = idx
		}(az, c)
	}
	wg.Wait()
	// Reported after the fan-in: warn is not assumed goroutine-safe.
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	for _, err := range errs {
		warn(err)
	}
	return out
}

// buildClusterInfo prices one cluster through its zone's machine classes.
// A nil or incomplete index yields a partial report, never an error: the
// replica counts and class names are always worth showing.
func buildClusterInfo(az string, kc *vitiv1alpha1.KubernetesCluster, classes machineClassIndex) clusterInfo {
	info := clusterInfo{
		AZ:                az,
		Namespace:         kc.Namespace,
		Name:              kc.Name,
		ClusterID:         kc.Spec.Cluster.ClusterId,
		Provider:          string(kc.Spec.Cluster.Provider),
		Environment:       kc.Spec.Cluster.Environment,
		Phase:             kc.Status.Phase,
		KubernetesVersion: kc.Spec.Topology.Version,
		NodePools:         []poolInfo{},
	}
	unknown := map[string]struct{}{}

	price := func(p *poolInfo) {
		if p.MachineClass == "" {
			return
		}
		size, ok := classes[p.MachineClass]
		if !ok {
			unknown[p.MachineClass] = struct{}{}
			return
		}
		total := size.times(p.Replicas)
		p.PerNode = &size
		p.Total = &total
	}

	cp := kc.Spec.Topology.ControlPlane
	info.ControlPlane = poolInfo{
		Role:         roleControlPlane,
		Replicas:     cp.Replicas,
		MachineClass: cp.MachineClass,
	}
	price(&info.ControlPlane)
	info.Totals.ControlPlane.addPool(info.ControlPlane)

	for _, np := range kc.Spec.Topology.Workers.NodePools {
		p := poolInfo{
			Role:         roleWorker,
			Name:         np.Name,
			Replicas:     np.Replicas,
			MachineClass: np.MachineClass,
		}
		if np.Autoscaling.Enabled {
			p.Autoscaling = &autoscalingInfo{
				MinReplicas: np.Autoscaling.MinReplicas,
				MaxReplicas: np.Autoscaling.MaxReplicas,
			}
		}
		price(&p)
		info.NodePools = append(info.NodePools, p)
		info.Totals.Workers.addPool(p)
	}

	info.Totals.Cluster.addPool(info.ControlPlane)
	for _, p := range info.NodePools {
		info.Totals.Cluster.addPool(p)
	}

	for name := range unknown {
		info.UnknownMachineClasses = append(info.UnknownMachineClasses, name)
	}
	sort.Strings(info.UnknownMachineClasses)
	return info
}

// buildClusterInfos prices every hit, looking each up in its own zone's
// classes: the same class name can mean different hardware in two zones.
func buildClusterInfos(hits []kcHit, classesByAZ map[string]machineClassIndex) []clusterInfo {
	infos := make([]clusterInfo, 0, len(hits))
	for _, h := range hits {
		infos = append(infos, buildClusterInfo(h.client.AZ.Name, h.cluster, classesByAZ[h.client.AZ.Name]))
	}
	return infos
}

// fmtMemory renders a quantity, or "?" when the size is unknown.
func fmtMemory(q *resource.Quantity) string {
	if q == nil {
		return "?"
	}
	return q.String()
}

// fmtCount renders a count, or "?" when unknown.
func fmtCount(n *int) string {
	if n == nil {
		return "?"
	}
	return strconv.Itoa(*n)
}

// hasGPU reports whether any pool in the report has GPUs, which decides
// whether the GPU columns are worth their width.
func hasGPU(info clusterInfo) bool {
	if info.ControlPlane.PerNode != nil && info.ControlPlane.PerNode.GPU > 0 {
		return true
	}
	for _, p := range info.NodePools {
		if p.PerNode != nil && p.PerNode.GPU > 0 {
			return true
		}
	}
	return false
}

// writeClusterInfo renders the human view of one cluster: a header line, a
// row per pool with per-node and total figures, then the totals split by
// role. Unknown sizes print as "?" and are called out below the tables, so a
// missing class never reads as zero capacity.
func writeClusterInfo(w io.Writer, info clusterInfo) {
	_, _ = fmt.Fprintf(w, "🎯 AZ: %s   namespace: %s   cluster: %s\n", info.AZ, info.Namespace, info.Name)
	_, _ = fmt.Fprintf(w, "   cluster id: %s   provider: %s   env: %s   phase: %s %s   k8s: %s\n",
		valueOrDash(info.ClusterID), valueOrDash(info.Provider), valueOrDash(info.Environment),
		phaseEmoji(info.Phase), valueOrDash(info.Phase), valueOrDash(info.KubernetesVersion))
	_, _ = fmt.Fprintln(w)

	gpu := hasGPU(info)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := "   ROLE\tPOOL\tREPLICAS\tMACHINE CLASS\tCPU/NODE\tMEM/NODE\tCPU TOTAL\tMEM TOTAL"
	if gpu {
		header += "\tGPU/NODE\tGPU TOTAL"
	}
	header += "\tAUTOSCALING"
	_, _ = fmt.Fprintln(tw, header)

	row := func(label string, p poolInfo) {
		var cpuNode, cpuTotal, gpuNode, gpuTotal *int
		var memNode, memTotal *resource.Quantity
		if p.PerNode != nil {
			cpuNode, memNode, gpuNode = &p.PerNode.CPU, &p.PerNode.Memory, &p.PerNode.GPU
		}
		if p.Total != nil {
			cpuTotal, memTotal, gpuTotal = &p.Total.CPU, &p.Total.Memory, &p.Total.GPU
		}
		autoscaling := "-"
		if p.Autoscaling != nil {
			autoscaling = fmt.Sprintf("%d-%d", p.Autoscaling.MinReplicas, p.Autoscaling.MaxReplicas)
		}
		cols := []string{
			"   " + label, valueOrDash(p.Name), strconv.Itoa(p.Replicas), valueOrDash(p.MachineClass),
			fmtCount(cpuNode), fmtMemory(memNode), fmtCount(cpuTotal), fmtMemory(memTotal),
		}
		if gpu {
			cols = append(cols, fmtCount(gpuNode), fmtCount(gpuTotal))
		}
		cols = append(cols, autoscaling)
		_, _ = fmt.Fprintln(tw, strings.Join(cols, "\t"))
	}
	row("control plane", info.ControlPlane)
	for _, p := range info.NodePools {
		row("worker", p)
	}
	_ = tw.Flush()

	_, _ = fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header = "   TOTALS\tNODES\tCPU\tMEMORY"
	if gpu {
		header += "\tGPU"
	}
	_, _ = fmt.Fprintln(tw, header)
	totalRow := func(label string, t resourceTotals) {
		cpu, mem := strconv.Itoa(t.CPU), t.Memory.String()
		g := strconv.Itoa(t.GPU)
		if t.Partial {
			// The figure is a floor: some nodes are of a size nobody could
			// tell us.
			cpu, mem, g = "≥"+cpu, "≥"+mem, "≥"+g
		}
		cols := []string{"   " + label, strconv.Itoa(t.Nodes), cpu, mem}
		if gpu {
			cols = append(cols, g)
		}
		_, _ = fmt.Fprintln(tw, strings.Join(cols, "\t"))
	}
	totalRow("control plane", info.Totals.ControlPlane)
	totalRow("workers", info.Totals.Workers)
	totalRow("cluster", info.Totals.Cluster)
	_ = tw.Flush()

	if len(info.UnknownMachineClasses) > 0 {
		_, _ = fmt.Fprintf(w, "\n⚠️  machine class %s not found in %s — per-node sizes unknown, totals are a lower bound\n",
			strings.Join(quoteAll(info.UnknownMachineClasses), ", "), info.AZ)
	}
}

// quoteAll quotes each name for an error or warning that lists several.
func quoteAll(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, strconv.Quote(n))
	}
	return out
}

// renderClusterInfos writes the report in the chosen format. The structured
// formats emit a single object when exactly one cluster was named and a list
// otherwise, so `viti kc info a -o json | jq .name` and
// `viti kc info --all -o json | jq '.[].name'` both do the obvious thing.
func renderClusterInfos(w io.Writer, infos []clusterInfo, format printer.Format, list bool) error {
	switch format {
	case printer.FormatJSON, printer.FormatYAML:
		var payload any = infos
		if !list && len(infos) == 1 {
			payload = infos[0]
		}
		if format == printer.FormatJSON {
			return printer.WriteJSONValue(w, payload)
		}
		return printer.WriteYAMLValue(w, payload)
	case printer.FormatTable:
		for i, info := range infos {
			if i > 0 {
				_, _ = fmt.Fprintln(w)
			}
			writeClusterInfo(w, info)
		}
		return nil
	default:
		return fmt.Errorf("unsupported output format %q for info (valid: json, yaml)", string(format))
	}
}

// selectClustersByName picks the hits for each requested name, in argument
// order, and returns the names that matched nothing. One name may match
// several clusters when it exists on more than one zone or namespace, and all
// of them are returned.
//
// A miss is not fatal here: the caller reports what was found and then fails
// on the misses, so one typo in a list of ten still yields nine reports and a
// non-zero exit that names the tenth.
func selectClustersByName(hits []kcHit, names []string) (found []kcHit, missing []string) {
	seen := map[string]struct{}{}
	for _, name := range names {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		matched := false
		for i := range hits {
			if hits[i].cluster.Name == name {
				found = append(found, hits[i])
				matched = true
			}
		}
		if !matched {
			missing = append(missing, name)
		}
	}
	return found, missing
}

// errMissingClusters is the failure for names that matched nothing. It is
// returned after the found clusters have been rendered, so the exit code
// still says the run was incomplete.
func errMissingClusters(missing []string) error {
	return fmt.Errorf("❌ no kubernetescluster named %s found on any availability zone",
		strings.Join(quoteAll(missing), ", "))
}

// pickClustersForInfo lets the operator mark any number of clusters.
func pickClustersForInfo(sess *picker.Session, hits []kcHit) ([]kcHit, error) {
	if len(hits) == 0 {
		return nil, errors.New("🤷 no kubernetesclusters found")
	}
	items := make([]picker.Item, 0, len(hits))
	for i := range hits {
		kc := hits[i].cluster
		columns := []string{
			hits[i].client.AZ.Name, kc.Namespace, kc.Name,
			valueOrDash(kc.Spec.Cluster.ClusterId),
			valueOrDash(string(kc.Spec.Cluster.Provider)),
			valueOrDash(kc.Spec.Cluster.Environment),
			valueOrDash(kc.Status.Phase),
			strconv.Itoa(kc.Spec.Topology.ControlPlane.Replicas),
			strconv.Itoa(len(kc.Spec.Topology.Workers.NodePools)),
		}
		items = append(items, picker.Item{
			Label:   strings.Join(columns, " "),
			Columns: columns,
			Value:   &hits[i],
		})
	}
	chosen, err := sess.SelectMulti(" Select clusters to describe (Tab marks, Enter confirms) ",
		[]string{"AZ", "NAMESPACE", "NAME", "CLUSTER ID", "PROVIDER", "ENV", "PHASE", "CP", "POOLS"}, items)
	if err != nil {
		if errors.Is(err, picker.ErrCancelled) {
			return nil, errors.New("aborted")
		}
		return nil, err
	}
	out := make([]kcHit, 0, len(chosen))
	for _, it := range chosen {
		got, ok := it.Value.(*kcHit)
		if !ok {
			return nil, fmt.Errorf("picker returned an unexpected item %T", it.Value)
		}
		out = append(out, *got)
	}
	return out, nil
}

var (
	kcInfoNamespace string
	kcInfoOutput    string
	kcInfoAll       bool
)

var kcInfoCmd = &cobra.Command{
	Use:     "info [name...]",
	Aliases: []string{"capacity"},
	Short:   "Show the node pools and compute capacity of one or more clusters",
	Long: `Describes what a KubernetesCluster is made of: its control plane and each
worker node pool with replica count and machine class, the CPU and memory one
node of that class provides, the total per pool, and the totals for the
control plane, the workers and the cluster as a whole.

Sizes come from the MachineClass objects of the cluster's own availability
zone. A class the zone does not define is reported as unknown ("?") and the
affected totals become a lower bound ("≥"), never a silent zero.

Several clusters can be described in one run: name them all, or pass --all
for every cluster in scope (narrow with -z and -n). With no name and a
terminal, clusters are picked interactively — Tab marks, Enter confirms.

-o json / -o yaml emit the same data for scripts. Exactly one name yields a
single object; several names, --all, or the picker yield a list.

Examples:
  viti kc info                          # pick one or more clusters
  viti kc info my-cluster
  viti kc info cluster-a cluster-b
  viti kc info --all -n team-a
  viti kc info my-cluster -o yaml
  viti kc info --all -o json | jq '.[] | {name, cpu: .totals.workers.cpu}'`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		format, err := printer.Parse(kcInfoOutput)
		if err != nil {
			return err
		}
		if format != printer.FormatTable && !format.IsStructured() {
			return fmt.Errorf("unsupported output format %q for info (valid: json, yaml)", kcInfoOutput)
		}
		if kcInfoAll && len(args) > 0 {
			return errors.New("--all and cluster names are exclusive — pass one or the other")
		}
		if len(args) == 0 && !kcInfoAll && !picker.Interactive() {
			return errors.New("no cluster given — pass one or more names (e.g. 'viti kc info my-cluster'), " +
				"--all for every cluster, or run in a terminal to pick interactively")
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		zones, err := kube.ResolveAvailabilityZones(AvailabilityZone())
		if err != nil {
			return err
		}
		clients, err := kube.ConnectAll(ctx, zones, true, warn)
		if err != nil {
			return err
		}
		all := collectClusters(ctx, clients, kcInfoNamespace)
		if err := sortByKeys(all, "az,namespace,name", kcComparators()); err != nil {
			return err
		}

		var (
			hits    []kcHit
			missing []string
		)
		switch {
		case kcInfoAll:
			hits = all
		case len(args) > 0:
			hits, missing = selectClustersByName(all, args)
			if len(hits) == 0 {
				return errMissingClusters(missing)
			}
		default:
			// The session is closed before anything prints: the report uses
			// plain stdout.
			sess, err := picker.NewSession()
			if err != nil {
				return err
			}
			hits, err = pickClustersForInfo(sess, all)
			sess.Close()
			if err != nil {
				return err
			}
		}

		out := cmd.OutOrStdout()
		if len(hits) == 0 {
			if format.IsStructured() {
				return renderClusterInfos(out, []clusterInfo{}, format, true)
			}
			_, _ = fmt.Fprintln(out, "🤷 no kubernetesclusters found")
			return nil
		}

		classesByAZ := indexMachineClassesByAZ(ctx, hits, warn)
		infos := buildClusterInfos(hits, classesByAZ)
		list := kcInfoAll || len(args) != 1
		if err := renderClusterInfos(out, infos, format, list); err != nil {
			return err
		}
		// Reported last, after every cluster that was found: the report is
		// complete for those, and the exit code says it is not for the rest.
		if len(missing) > 0 {
			if !format.IsStructured() {
				_, _ = fmt.Fprintln(out)
			}
			return errMissingClusters(missing)
		}
		return nil
	},
}

func init() {
	kcInfoCmd.Flags().StringVarP(&kcInfoNamespace, "namespace", "n", "",
		"namespace of the KubernetesCluster(s)")
	kcInfoCmd.Flags().StringVarP(&kcInfoOutput, "output", "o", "",
		"output format: json, yaml (default: readable report)")
	kcInfoCmd.Flags().BoolVar(&kcInfoAll, "all", false,
		"describe every cluster in scope instead of naming them")

	kubernetesClusterCmd.AddCommand(kcInfoCmd)
}
