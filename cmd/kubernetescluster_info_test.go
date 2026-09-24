package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/printer"
	"github.com/vitistack/vitictl/internal/settings"
)

// infoTestClasses is the zone used by the info tests: two ordinary classes
// and one with GPUs.
func infoTestClasses() machineClassIndex {
	return machineClassIndex{
		"mcpu":  {CPU: 4, Memory: resource.MustParse("8Gi")},
		"lcpu":  {CPU: 8, Memory: resource.MustParse("32Gi")},
		"xlgpu": {CPU: 16, Memory: resource.MustParse("64Gi"), GPU: 2},
	}
}

// infoTestCluster declares a three-node control plane and two worker pools,
// one of them autoscaled.
func infoTestCluster() *vitiv1alpha1.KubernetesCluster {
	kc := &vitiv1alpha1.KubernetesCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "cluster-a"},
	}
	kc.Spec.Cluster.ClusterId = "cluster-a-id"
	kc.Spec.Cluster.Provider = vitiv1alpha1.KubernetesProviderTypeTalos
	kc.Spec.Cluster.Environment = "prod"
	kc.Spec.Topology.Version = "1.31.2"
	kc.Spec.Topology.ControlPlane.Replicas = 3
	kc.Spec.Topology.ControlPlane.MachineClass = "mcpu"
	kc.Spec.Topology.Workers.NodePools = []vitiv1alpha1.KubernetesClusterNodePool{
		{Name: "workers", Replicas: 5, MachineClass: "lcpu"},
		{Name: "gpu", Replicas: 2, MachineClass: "xlgpu"},
	}
	kc.Spec.Topology.Workers.NodePools[1].Autoscaling.Enabled = true
	kc.Spec.Topology.Workers.NodePools[1].Autoscaling.MinReplicas = 2
	kc.Spec.Topology.Workers.NodePools[1].Autoscaling.MaxReplicas = 6
	kc.Status.Phase = "Running"
	return kc
}

func TestBuildClusterInfoPricesEveryPool(t *testing.T) {
	info := buildClusterInfo("az-a", infoTestCluster(), infoTestClasses())

	cp := info.ControlPlane
	if cp.Role != roleControlPlane || cp.Replicas != 3 || cp.MachineClass != "mcpu" {
		t.Fatalf("control plane = %+v, want 3 x mcpu", cp)
	}
	if cp.PerNode == nil || cp.PerNode.CPU != 4 || cp.PerNode.Memory.String() != "8Gi" {
		t.Errorf("control plane per node = %+v, want 4 cores / 8Gi", cp.PerNode)
	}
	if cp.Total == nil || cp.Total.CPU != 12 || cp.Total.Memory.String() != "24Gi" {
		t.Errorf("control plane total = %+v, want 12 cores / 24Gi", cp.Total)
	}

	if len(info.NodePools) != 2 {
		t.Fatalf("node pools = %d, want 2", len(info.NodePools))
	}
	workers := info.NodePools[0]
	if workers.Role != roleWorker || workers.Name != "workers" {
		t.Errorf("first pool = %+v, want worker pool \"workers\"", workers)
	}
	if workers.Total == nil || workers.Total.CPU != 40 || workers.Total.Memory.String() != "160Gi" {
		t.Errorf("workers total = %+v, want 40 cores / 160Gi", workers.Total)
	}
	if workers.Autoscaling != nil {
		t.Error("a pool without autoscaling must not report an autoscaling block")
	}
	gpu := info.NodePools[1]
	if gpu.Autoscaling == nil || gpu.Autoscaling.MinReplicas != 2 || gpu.Autoscaling.MaxReplicas != 6 {
		t.Errorf("gpu autoscaling = %+v, want 2-6", gpu.Autoscaling)
	}
	if gpu.Total == nil || gpu.Total.GPU != 4 {
		t.Errorf("gpu pool total = %+v, want 4 GPUs", gpu.Total)
	}

	if len(info.UnknownMachineClasses) != 0 {
		t.Errorf("unknown classes = %v, want none", info.UnknownMachineClasses)
	}
}

func TestBuildClusterInfoSplitsTotalsByRole(t *testing.T) {
	info := buildClusterInfo("az-a", infoTestCluster(), infoTestClasses())

	check := func(label string, got resourceTotals, nodes, cpu int, mem string, gpu int) {
		t.Helper()
		if got.Partial {
			t.Errorf("%s totals must not be partial when every class resolved", label)
		}
		if got.Nodes != nodes || got.CPU != cpu || got.Memory.String() != mem || got.GPU != gpu {
			t.Errorf("%s totals = nodes %d cpu %d mem %s gpu %d, want %d / %d / %s / %d",
				label, got.Nodes, got.CPU, got.Memory.String(), got.GPU, nodes, cpu, mem, gpu)
		}
	}
	check("control plane", info.Totals.ControlPlane, 3, 12, "24Gi", 0)
	check("workers", info.Totals.Workers, 7, 72, "288Gi", 4)
	check("cluster", info.Totals.Cluster, 10, 84, "312Gi", 4)
}

func TestBuildClusterInfoMarksUnknownClassesAsPartial(t *testing.T) {
	classes := infoTestClasses()
	delete(classes, "lcpu")
	info := buildClusterInfo("az-a", infoTestCluster(), classes)

	workers := info.NodePools[0]
	if workers.PerNode != nil || workers.Total != nil {
		t.Errorf("an unknown class must leave the sizes absent, got per node %+v total %+v", workers.PerNode, workers.Total)
	}
	if workers.MachineClass != "lcpu" || workers.Replicas != 5 {
		t.Errorf("class name and replicas must survive an unknown class, got %+v", workers)
	}
	if got := info.UnknownMachineClasses; len(got) != 1 || got[0] != "lcpu" {
		t.Errorf("unknown classes = %v, want [lcpu]", got)
	}

	if !info.Totals.Workers.Partial || !info.Totals.Cluster.Partial {
		t.Error("totals touching the unknown pool must be marked partial")
	}
	if info.Totals.ControlPlane.Partial {
		t.Error("control plane totals must not be partial: its class resolved")
	}
	// Nodes are always exact; capacity excludes the unpriced pool.
	if info.Totals.Workers.Nodes != 7 || info.Totals.Workers.CPU != 32 {
		t.Errorf("workers totals = %d nodes / %d cpu, want 7 / 32", info.Totals.Workers.Nodes, info.Totals.Workers.CPU)
	}
}

func TestBuildClusterInfoWithoutAnyClassesStillReportsTopology(t *testing.T) {
	info := buildClusterInfo("az-a", infoTestCluster(), nil)
	if info.ControlPlane.Replicas != 3 || len(info.NodePools) != 2 {
		t.Fatalf("topology must render without a class index, got %+v", info)
	}
	if info.Totals.Cluster.Nodes != 10 || !info.Totals.Cluster.Partial {
		t.Errorf("cluster totals = %+v, want 10 nodes, partial", info.Totals.Cluster)
	}
	if len(info.UnknownMachineClasses) != 3 {
		t.Errorf("unknown classes = %v, want all three", info.UnknownMachineClasses)
	}
}

func TestBuildClusterInfoTreatsEmptyClassAsUnpricedNotUnknown(t *testing.T) {
	kc := infoTestCluster()
	kc.Spec.Topology.Workers.NodePools[0].MachineClass = ""
	info := buildClusterInfo("az-a", kc, infoTestClasses())
	if len(info.UnknownMachineClasses) != 0 {
		t.Errorf("an empty class is not a missing one, got unknown = %v", info.UnknownMachineClasses)
	}
	if !info.Totals.Workers.Partial {
		t.Error("a pool with no class still leaves the totals partial")
	}
}

func TestSelectClustersByNameReportsEveryMissingName(t *testing.T) {
	az := &kube.Client{AZ: settings.AvailabilityZone{Name: "az-a"}}
	a := infoTestCluster()
	b := infoTestCluster()
	b.Name = "cluster-b"
	hits := []kcHit{{client: az, cluster: a}, {client: az, cluster: b}}

	got, missing := selectClustersByName(hits, []string{"cluster-b", "cluster-a", "cluster-b"})
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want none", missing)
	}
	if len(got) != 2 || got[0].cluster.Name != "cluster-b" || got[1].cluster.Name != "cluster-a" {
		t.Errorf("selection must follow the argument order and drop duplicates, got %d hits", len(got))
	}

	// One miss must not cost the hits: the found cluster is still returned
	// so it can be reported before the run fails on the misses.
	got, missing = selectClustersByName(hits, []string{"cluster-a", "nope", "missing"})
	if len(got) != 1 || got[0].cluster.Name != "cluster-a" {
		t.Errorf("the matched cluster must still be returned alongside the misses, got %d hits", len(got))
	}
	if len(missing) != 2 || missing[0] != "nope" || missing[1] != "missing" {
		t.Errorf("missing = %v, want [nope missing] in argument order", missing)
	}

	err := errMissingClusters(missing)
	for _, want := range []string{`"nope"`, `"missing"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name %s", err, want)
		}
	}
	if strings.Contains(err.Error(), `"cluster-a"`) {
		t.Errorf("error %q must not list the name that was found", err)
	}
}

func TestSelectClustersByNameReturnsEveryZoneHoldingTheName(t *testing.T) {
	a := infoTestCluster()
	b := infoTestCluster()
	hits := []kcHit{
		{client: &kube.Client{AZ: settings.AvailabilityZone{Name: "az-a"}}, cluster: a},
		{client: &kube.Client{AZ: settings.AvailabilityZone{Name: "az-b"}}, cluster: b},
	}
	got, missing := selectClustersByName(hits, []string{"cluster-a"})
	if len(missing) != 0 {
		t.Fatal(missing)
	}
	if len(got) != 2 {
		t.Errorf("a read-only report shows every zone holding the name, got %d", len(got))
	}
}

func TestRenderClusterInfosJSONShapeFollowsHowManyWereAsked(t *testing.T) {
	info := buildClusterInfo("az-a", infoTestCluster(), infoTestClasses())

	var single bytes.Buffer
	if err := renderClusterInfos(&single, []clusterInfo{info}, printer.FormatJSON, false); err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(single.Bytes(), &obj); err != nil {
		t.Fatalf("one named cluster must render as an object: %v\n%s", err, single.String())
	}
	if obj["name"] != "cluster-a" {
		t.Errorf("name = %v, want cluster-a", obj["name"])
	}
	totals, _ := obj["totals"].(map[string]any)
	workers, _ := totals["workers"].(map[string]any)
	if workers["memory"] != "288Gi" {
		t.Errorf("totals.workers.memory = %v, want the quantity string \"288Gi\"", workers["memory"])
	}
	if _, present := obj["unknownMachineClasses"]; present {
		t.Error("unknownMachineClasses must be omitted when empty")
	}

	var list bytes.Buffer
	if err := renderClusterInfos(&list, []clusterInfo{info}, printer.FormatJSON, true); err != nil {
		t.Fatal(err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(list.Bytes(), &arr); err != nil {
		t.Fatalf("--all must render as a list even for one cluster: %v\n%s", err, list.String())
	}
	if len(arr) != 1 {
		t.Errorf("list length = %d, want 1", len(arr))
	}
}

func TestRenderClusterInfosYAMLUsesJSONFieldNames(t *testing.T) {
	info := buildClusterInfo("az-a", infoTestCluster(), infoTestClasses())
	var out bytes.Buffer
	if err := renderClusterInfos(&out, []clusterInfo{info}, printer.FormatYAML, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"controlPlane:", "nodePools:", "machineClass: lcpu", "memory: 160Gi", "minReplicas: 2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("yaml must contain %q:\n%s", want, out.String())
		}
	}
}

func TestRenderClusterInfosRejectsTableOnlyFormats(t *testing.T) {
	err := renderClusterInfos(&bytes.Buffer{}, nil, printer.FormatWide, true)
	if err == nil {
		t.Fatal("wide is a list-table format and means nothing for the info report")
	}
}

func TestWriteClusterInfoShowsPoolsTotalsAndUnknowns(t *testing.T) {
	classes := infoTestClasses()
	delete(classes, "mcpu")
	info := buildClusterInfo("az-a", infoTestCluster(), classes)

	var out bytes.Buffer
	writeClusterInfo(&out, info)
	got := out.String()

	for _, want := range []string{
		"cluster: cluster-a",
		"k8s: 1.31.2",
		"control plane", "workers", "gpu",
		"MACHINE CLASS", "CPU/NODE", "MEM/NODE", "CPU TOTAL", "MEM TOTAL",
		"GPU/NODE", "GPU TOTAL", // shown because the gpu pool has GPUs
		"160Gi", "2-6",
		"TOTALS", "NODES",
		`machine class "mcpu" not found in az-a`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report must contain %q:\n%s", want, got)
		}
	}
	// The unpriced control plane shows "?" and the totals it touches are a
	// lower bound, not a zero.
	lines := strings.Split(got, "\n")
	var cpRow, cpTotal, clusterTotal string
	for _, l := range lines {
		switch {
		case strings.Contains(l, "control plane") && strings.Contains(l, "mcpu"):
			cpRow = l
		case strings.Contains(l, "control plane") && !strings.Contains(l, "mcpu"):
			cpTotal = l
		case strings.HasPrefix(strings.TrimSpace(l), "cluster ") || strings.HasPrefix(strings.TrimSpace(l), "cluster\t"):
			clusterTotal = l
		}
	}
	if !strings.Contains(cpRow, "?") {
		t.Errorf("control plane row must show ? for unknown sizes: %q", cpRow)
	}
	if !strings.Contains(cpTotal, "≥0") {
		t.Errorf("control plane totals must be a lower bound, got %q", cpTotal)
	}
	if !strings.Contains(clusterTotal, "≥72") || !strings.Contains(clusterTotal, "10") {
		t.Errorf("cluster totals must count 10 nodes and at least 72 cores, got %q", clusterTotal)
	}
}

func TestWriteClusterInfoHidesGPUColumnsWithoutGPUs(t *testing.T) {
	kc := infoTestCluster()
	kc.Spec.Topology.Workers.NodePools = kc.Spec.Topology.Workers.NodePools[:1]
	info := buildClusterInfo("az-a", kc, infoTestClasses())
	var out bytes.Buffer
	writeClusterInfo(&out, info)
	if strings.Contains(out.String(), "GPU") {
		t.Errorf("GPU columns must only appear when some pool has GPUs:\n%s", out.String())
	}
}

func TestIndexMachineClassesByAZReadsEachZoneOnce(t *testing.T) {
	sch := runtime.NewScheme()
	if err := vitiv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	mc := func(name string, cores uint, mem string) *vitiv1alpha1.MachineClass {
		m := &vitiv1alpha1.MachineClass{ObjectMeta: metav1.ObjectMeta{Name: name}}
		m.Spec.CPU.Cores = cores
		m.Spec.Memory.Quantity = resource.MustParse(mem)
		m.Spec.Enabled = false // disabled classes still size the nodes running on them
		return m
	}
	azA := &kube.Client{
		AZ:   settings.AvailabilityZone{Name: "az-a"},
		Ctrl: fake.NewClientBuilder().WithScheme(sch).WithObjects(mc("small", 2, "4Gi")).Build(),
	}
	azB := &kube.Client{
		AZ:   settings.AvailabilityZone{Name: "az-b"},
		Ctrl: fake.NewClientBuilder().WithScheme(sch).WithObjects(mc("small", 4, "16Gi")).Build(),
	}
	kcA, kcB := infoTestCluster(), infoTestCluster()
	hits := []kcHit{{client: azA, cluster: kcA}, {client: azA, cluster: kcB}, {client: azB, cluster: kcA}}

	var warned []error
	got := indexMachineClassesByAZ(t.Context(), hits, func(err error) { warned = append(warned, err) })
	if len(warned) != 0 {
		t.Fatalf("unexpected warnings: %v", warned)
	}
	if got["az-a"]["small"].CPU != 2 || got["az-b"]["small"].CPU != 4 {
		t.Errorf("the same class name must resolve per zone, got a=%+v b=%+v", got["az-a"]["small"], got["az-b"]["small"])
	}
	if mem := got["az-a"]["small"].Memory; mem.String() != "4Gi" {
		t.Errorf("memory = %s, want 4Gi", mem.String())
	}
}

func TestIndexMachineClassesByAZWarnsAndContinuesWhenAZoneFails(t *testing.T) {
	sch := runtime.NewScheme()
	if err := vitiv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	okZone := &kube.Client{
		AZ:   settings.AvailabilityZone{Name: "az-ok"},
		Ctrl: fake.NewClientBuilder().WithScheme(sch).Build(),
	}
	// A scheme without the vitistack types makes List fail for that zone.
	brokenZone := &kube.Client{
		AZ:   settings.AvailabilityZone{Name: "az-broken"},
		Ctrl: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build(),
	}
	hits := []kcHit{{client: okZone, cluster: infoTestCluster()}, {client: brokenZone, cluster: infoTestCluster()}}

	var warned []error
	got := indexMachineClassesByAZ(t.Context(), hits, func(err error) { warned = append(warned, err) })
	if len(warned) != 1 || !strings.Contains(warned[0].Error(), "az-broken") {
		t.Errorf("warnings = %v, want one naming az-broken", warned)
	}
	if _, ok := got["az-ok"]; !ok {
		t.Error("the healthy zone must still be indexed")
	}
	if _, ok := got["az-broken"]; ok {
		t.Error("the failed zone must be absent, not present and empty")
	}
	if errors.Join(warned...) == nil {
		t.Error("expected a joined error for the sake of the warning path")
	}
}
