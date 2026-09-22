package cmd

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

// richPool builds a pool with every clonable field set, so the clone tests
// can prove each one is carried over.
func richPool(name string, replicas int) vitiv1alpha1.KubernetesClusterNodePool {
	p := pool(name, replicas)
	p.MachineClass = "medium"
	p.Provider = vitiv1alpha1.KubernetesProviderTypeTalos
	p.Version = "1.36.2"
	p.Architecture = "amd64"
	p.Autoscaling.Enabled = true
	p.Autoscaling.MinReplicas = 1
	p.Autoscaling.MaxReplicas = 5
	p.Autoscaling.ScalingRules = []string{"cpu"}
	p.Metadata.Labels = map[string]string{"environment": "test"}
	p.Metadata.Annotations = map[string]string{"region": "east"}
	p.Taint = []vitiv1alpha1.KubernetesClusterTaint{{Key: "gpu", Value: "true", Effect: "NoSchedule"}}
	p.Storage = []vitiv1alpha1.KubernetesClusterStorage{{Class: "standard", Path: "/var/lib/vitistack/kubevirt", Size: "100Gi"}}
	return p
}

func TestNewPoolNameMatchesTheGeneratedPattern(t *testing.T) {
	name, err := newPoolName(nil)
	if err != nil {
		t.Fatalf("newPoolName: %v", err)
	}
	if !regexp.MustCompile(`^workers-[0-9a-f]{8}$`).MatchString(name) {
		t.Errorf("generated name %q, want workers-<8 hex>", name)
	}
	if err := validatePoolName(name, nil); err != nil {
		t.Errorf("a generated name must always be a valid pool name, got: %v", err)
	}
}

func TestNewPoolNamesAreUnique(t *testing.T) {
	a, err := newPoolName(nil)
	if err != nil {
		t.Fatalf("newPoolName: %v", err)
	}
	b, err := newPoolName(map[string]struct{}{a: {}})
	if err != nil {
		t.Fatalf("newPoolName: %v", err)
	}
	if a == b {
		t.Errorf("two generated names are both %q — the taken set was ignored", a)
	}
}

func TestCloneNodePoolReplicatesTheTemplate(t *testing.T) {
	tpl := richPool("workers", 12)
	got := cloneNodePool(tpl, "workers-abcd1234", "xlcpu", 3)

	if got.Name != "workers-abcd1234" || got.MachineClass != "xlcpu" || got.Replicas != 3 {
		t.Errorf("overrides not applied: got name=%q class=%q replicas=%d", got.Name, got.MachineClass, got.Replicas)
	}
	if got.Provider != tpl.Provider || got.Version != tpl.Version || got.Architecture != tpl.Architecture {
		t.Errorf("provider/version/architecture not carried over: got %+v", got)
	}
	if !got.Autoscaling.Enabled || got.Autoscaling.MinReplicas != 1 || got.Autoscaling.MaxReplicas != 5 {
		t.Errorf("autoscaling not carried over: got %+v", got.Autoscaling)
	}
	if len(got.Autoscaling.ScalingRules) != 1 || got.Autoscaling.ScalingRules[0] != "cpu" {
		t.Errorf("scaling rules not carried over: got %v", got.Autoscaling.ScalingRules)
	}
	if got.Metadata.Labels["environment"] != "test" || got.Metadata.Annotations["region"] != "east" {
		t.Errorf("metadata not carried over: got %+v", got.Metadata)
	}
	if len(got.Taint) != 1 || got.Taint[0].Key != "gpu" {
		t.Errorf("taints not carried over: got %v", got.Taint)
	}
	if len(got.Storage) != 1 || got.Storage[0].Size != "100Gi" {
		t.Errorf("storage not carried over: got %v", got.Storage)
	}
}

func TestCloneNodePoolDoesNotShareSlicesWithTheTemplate(t *testing.T) {
	tpl := richPool("workers", 12)
	got := cloneNodePool(tpl, "clone", "xlcpu", 3)

	got.Taint[0].Key = "changed"
	got.Storage[0].Size = "1Gi"
	got.Autoscaling.ScalingRules[0] = "memory"
	got.Metadata.Labels["environment"] = "prod"

	if tpl.Taint[0].Key != "gpu" || tpl.Storage[0].Size != "100Gi" ||
		tpl.Autoscaling.ScalingRules[0] != "cpu" || tpl.Metadata.Labels["environment"] != "test" {
		t.Errorf("mutating the clone changed the template: %+v", tpl)
	}
}

func TestCheckPoolEditFlagsRejectsAddAndDeleteTogether(t *testing.T) {
	err := checkPoolEditFlags(poolEditFlags{add: true, deletePool: "gpu"}, "", nil, true)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("add+delete must be refused, got: %v", err)
	}
}

func TestCheckPoolEditFlagsRejectsCombiningWithReplicaFlags(t *testing.T) {
	if err := checkPoolEditFlags(poolEditFlags{add: true}, "3", nil, true); err == nil {
		t.Error("--add-nodepool with --controlplane must be refused")
	}
	if err := checkPoolEditFlags(poolEditFlags{deletePool: "gpu"}, "", []string{"workers=5"}, true); err == nil {
		t.Error("--delete-nodepool with --nodepool must be refused")
	}
}

func TestCheckPoolEditFlagsRejectsAddOnlyFlagsWithoutAdd(t *testing.T) {
	for _, f := range []poolEditFlags{
		{fromPool: "workers"},
		{machineClass: "xlcpu"},
		{replicas: "3"},
		{name: "workers-2"},
		{deletePool: "gpu", machineClass: "xlcpu"},
	} {
		if err := checkPoolEditFlags(f, "", nil, true); err == nil {
			t.Errorf("add-only flags without --add-nodepool must be refused, flags: %+v", f)
		}
	}
}

func TestCheckPoolEditFlagsRejectsAnEmptyDeleteName(t *testing.T) {
	err := checkPoolEditFlags(poolEditFlags{deleteChanged: true}, "", nil, true)
	if err == nil || !strings.Contains(err.Error(), "empty pool name") {
		t.Errorf("--delete-nodepool \"\" must be refused, got: %v", err)
	}
}

func TestCheckPoolEditFlagsJudgesTheReplicaCount(t *testing.T) {
	for _, replicas := range []string{"+2", "-1", "0", "abc"} {
		f := poolEditFlags{add: true, machineClass: "xlcpu", replicas: replicas}
		if err := checkPoolEditFlags(f, "", nil, true); err == nil {
			t.Errorf("--replicas %s must be refused for a new pool", replicas)
		}
	}
	f := poolEditFlags{add: true, machineClass: "xlcpu", replicas: "3"}
	if err := checkPoolEditFlags(f, "", nil, true); err != nil {
		t.Errorf("--replicas 3 must be accepted, got: %v", err)
	}
}

func TestCheckPoolEditFlagsRequiresTheAddInputsWithoutATerminal(t *testing.T) {
	if err := checkPoolEditFlags(poolEditFlags{add: true, replicas: "3"}, "", nil, false); err == nil {
		t.Error("non-interactive add without --machineclass must be refused")
	}
	if err := checkPoolEditFlags(poolEditFlags{add: true, machineClass: "xlcpu"}, "", nil, false); err == nil {
		t.Error("non-interactive add without --replicas must be refused")
	}
	f := poolEditFlags{add: true, machineClass: "xlcpu", replicas: "3"}
	if err := checkPoolEditFlags(f, "", nil, false); err != nil {
		t.Errorf("non-interactive add with both inputs must be accepted, got: %v", err)
	}
}

func TestCheckPoolEditFlagsAcceptsAQuietRun(t *testing.T) {
	// No pool-edit flags at all — the ordinary scale path must be untouched.
	if err := checkPoolEditFlags(poolEditFlags{}, "3", []string{"workers=5"}, false); err != nil {
		t.Errorf("a plain scale run must pass the pool-edit check, got: %v", err)
	}
}

func TestValidatePoolNameRejectsDuplicatesAndBadLabels(t *testing.T) {
	taken := map[string]struct{}{"workers": {}}
	if err := validatePoolName("workers", taken); err == nil {
		t.Error("a duplicate name must be refused")
	}
	if err := validatePoolName("Bad_Name", nil); err == nil {
		t.Error("a name that is not a DNS-1123 label must be refused")
	}
	if err := validatePoolName("workers-2", taken); err != nil {
		t.Errorf("a fresh valid name must be accepted, got: %v", err)
	}
}

func TestResolveTemplatePoolUsesTheOnlyPool(t *testing.T) {
	pools := []vitiv1alpha1.KubernetesClusterNodePool{richPool("workers", 12)}
	tpl, needPicker, err := resolveTemplatePool(pools, "", false)
	if err != nil || needPicker {
		t.Fatalf("got needPicker=%v err=%v, want the single pool without a picker", needPicker, err)
	}
	if tpl.Name != "workers" {
		t.Errorf("got template %q, want workers", tpl.Name)
	}
}

func TestResolveTemplatePoolHonoursFromPool(t *testing.T) {
	pools := []vitiv1alpha1.KubernetesClusterNodePool{pool("workers", 12), pool("gpu", 2)}
	tpl, _, err := resolveTemplatePool(pools, "gpu", false)
	if err != nil || tpl.Name != "gpu" {
		t.Errorf("got tpl=%v err=%v, want the gpu pool", tpl, err)
	}
	_, _, err = resolveTemplatePool(pools, "nope", false)
	if err == nil || !strings.Contains(err.Error(), `"workers"`) {
		t.Errorf("a miss must list the pools the cluster has, got: %v", err)
	}
}

func TestResolveTemplatePoolNeedsAPickerOnlyWithATerminal(t *testing.T) {
	pools := []vitiv1alpha1.KubernetesClusterNodePool{pool("workers", 12), pool("gpu", 2)}
	_, needPicker, err := resolveTemplatePool(pools, "", true)
	if err != nil || !needPicker {
		t.Errorf("several pools with a terminal must ask for the picker, got needPicker=%v err=%v", needPicker, err)
	}
	_, _, err = resolveTemplatePool(pools, "", false)
	if err == nil || !strings.Contains(err.Error(), "--from-pool") {
		t.Errorf("several pools without a terminal must point at --from-pool, got: %v", err)
	}
}

func TestResolveTemplatePoolRefusesAClusterWithoutPools(t *testing.T) {
	if _, _, err := resolveTemplatePool(nil, "", true); err == nil {
		t.Error("a cluster with no pools has nothing to clone — must be refused")
	}
}

func TestCheckPoolDeleteRefusesTheLastPool(t *testing.T) {
	pools := []vitiv1alpha1.KubernetesClusterNodePool{pool("workers", 12)}
	_, err := checkPoolDelete(pools, "workers")
	if err == nil || !strings.Contains(err.Error(), "last worker pool") {
		t.Errorf("deleting the only pool must be refused with the reason, got: %v", err)
	}
}

func TestCheckPoolDeleteNamesThePoolsOnAMiss(t *testing.T) {
	pools := []vitiv1alpha1.KubernetesClusterNodePool{pool("workers", 12), pool("gpu", 2)}
	_, err := checkPoolDelete(pools, "nope")
	if err == nil || !strings.Contains(err.Error(), `"workers"`) || !strings.Contains(err.Error(), `"gpu"`) {
		t.Errorf("a miss must list the pools the cluster has, got: %v", err)
	}
}

func TestCheckPoolDeleteReturnsTheRightIndex(t *testing.T) {
	pools := []vitiv1alpha1.KubernetesClusterNodePool{pool("workers", 12), pool("gpu", 2)}
	i, err := checkPoolDelete(pools, "gpu")
	if err != nil || i != 1 {
		t.Errorf("got index=%d err=%v, want index 1", i, err)
	}
}

func TestCheckPoolDeleteRefusesAClusterWithoutPools(t *testing.T) {
	if _, err := checkPoolDelete(nil, "workers"); err == nil {
		t.Error("deleting from a cluster with no pools must be refused")
	}
}

func TestWritePoolEditPlanForAnAdd(t *testing.T) {
	pools := []vitiv1alpha1.KubernetesClusterNodePool{richPool("workers", 12)}
	newPool := cloneNodePool(pools[0], "workers-abcd1234", "xlcpu", 3)
	edit := poolEdit{kind: poolEditAdd, newPool: newPool, template: "workers"}

	var buf bytes.Buffer
	if err := writePoolEditPlan(&buf, "az1", "team-a", "my-cluster", edit, pools); err != nil {
		t.Fatalf("writePoolEditPlan: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"will append to spec.topology.workers.nodePools",
		"- architecture: amd64",
		"machineClass: xlcpu",
		"name: workers-abcd1234",
		"replicas: 3",
		"size: 100Gi",
		"autoscaling enabled", // richPool's template autoscales; the clone inherits it
	} {
		if !strings.Contains(out, want) {
			t.Errorf("add plan is missing %q:\n%s", want, out)
		}
	}
	// The resulting table must show both the template and the new pool.
	if !strings.Contains(out, "workers ") || !strings.Contains(out, "workers-abcd1234 ") {
		t.Errorf("the after-table must list template and clone:\n%s", out)
	}
}

func TestWritePoolEditPlanForADelete(t *testing.T) {
	pools := []vitiv1alpha1.KubernetesClusterNodePool{richPool("workers", 12), richPool("gpu", 2)}
	edit := poolEdit{kind: poolEditDelete, poolName: "gpu", poolIndex: 1}

	var buf bytes.Buffer
	if err := writePoolEditPlan(&buf, "az1", "team-a", "my-cluster", edit, pools); err != nil {
		t.Fatalf("writePoolEditPlan: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"will remove from spec.topology.workers.nodePools",
		"name: gpu",
		"deletes its worker machines",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("delete plan is missing %q:\n%s", want, out)
		}
	}
	// The after-table must not list the removed pool. The YAML block above it
	// does contain "gpu", so check the table region specifically.
	table := out[strings.Index(out, "node pools after the change"):]
	if strings.Contains(table, "gpu") {
		t.Errorf("the after-table must not list the removed pool:\n%s", table)
	}
	if !strings.Contains(table, "workers") {
		t.Errorf("the after-table must keep the remaining pool:\n%s", table)
	}
}

func TestWritePoolEditPlanRefusesADriftedDeleteIndex(t *testing.T) {
	pools := []vitiv1alpha1.KubernetesClusterNodePool{pool("workers", 12), pool("gpu", 2)}
	edit := poolEdit{kind: poolEditDelete, poolName: "gpu", poolIndex: 0}
	var buf bytes.Buffer
	if err := writePoolEditPlan(&buf, "az1", "team-a", "my-cluster", edit, pools); err == nil {
		t.Error("an index that no longer matches the pool name must be refused")
	}
}

func TestParseNewPoolReplicas(t *testing.T) {
	if n, err := parseNewPoolReplicas("3"); err != nil || n != 3 {
		t.Errorf("got %d, %v — want 3", n, err)
	}
	for _, bad := range []string{"+2", "-1", "0", ""} {
		if _, err := parseNewPoolReplicas(bad); err == nil {
			t.Errorf("%q must be refused for a new pool", bad)
		}
	}
}

func TestValidateNewPoolReplicasMirrorsTheParser(t *testing.T) {
	if err := validateNewPoolReplicas("3"); err != nil {
		t.Errorf("3 must be accepted for a new pool, got: %v", err)
	}
	for _, bad := range []string{"+2", "-1", "0", "", "lots"} {
		if err := validateNewPoolReplicas(bad); err == nil {
			t.Errorf("%q must be refused for a new pool", bad)
		}
	}
}

func TestMachineClassesSortByNameAlone(t *testing.T) {
	mc := func(name, category string, cores uint) vitiv1alpha1.MachineClass {
		var m vitiv1alpha1.MachineClass
		m.Name = name
		m.Spec.Category = category
		m.Spec.CPU.Cores = cores
		return m
	}
	classes := []vitiv1alpha1.MachineClass{
		mc("xlcpu", "Compute", 32),
		mc("best-effort-cpu-medium-big", "BestEffort", 8),
		mc("medium", "Standard", 4),
	}
	sortMachineClassesByName(classes)
	want := []string{"best-effort-cpu-medium-big", "medium", "xlcpu"}
	for i, w := range want {
		if classes[i].Name != w {
			t.Fatalf("position %d is %q, want %q — the list must read in name order regardless of category or size",
				i, classes[i].Name, w)
		}
	}
}

func TestValidateMachineClassRefusesAnEmptyAnswer(t *testing.T) {
	if err := validateMachineClass(""); err == nil {
		t.Error("an empty machine class must be refused")
	}
	if err := validateMachineClass("xlcpu"); err != nil {
		t.Errorf("a named machine class must be accepted, got: %v", err)
	}
}

func TestPoolEditFlagsParseAndReset(t *testing.T) {
	resetScaleFlags(t)
	if err := kcScaleCmd.ParseFlags([]string{
		"--add-nodepool", "--from-pool", "workers", "--machineclass", "xlcpu",
		"--replicas", "3", "--name", "workers-2",
	}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	f := currentPoolEditFlags(kcScaleCmd)
	if !f.add || f.fromPool != "workers" || f.machineClass != "xlcpu" || f.replicas != "3" || f.name != "workers-2" {
		t.Errorf("parsed flags = %+v, want the add flags round-tripped", f)
	}
	if !poolEditRequested() {
		t.Error("poolEditRequested must see --add-nodepool")
	}

	resetScaleFlags(t)
	if poolEditRequested() {
		t.Error("resetScaleFlags must clear the pool-edit flags")
	}
	if err := kcScaleCmd.ParseFlags([]string{"--delete-nodepool", "gpu"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	f = currentPoolEditFlags(kcScaleCmd)
	if f.deletePool != "gpu" || !f.deleteChanged || !poolEditRequested() {
		t.Errorf("parsed flags = %+v, want the delete flag round-tripped", f)
	}
	resetScaleFlags(t)
}

func TestPoolEditApplyAddAppends(t *testing.T) {
	kc := scaleTestCluster(3, pool("workers", 12))
	edit := poolEdit{kind: poolEditAdd, newPool: richPool("workers-abcd1234", 3)}
	if err := edit.apply(kc); err != nil {
		t.Fatalf("apply: %v", err)
	}
	pools := kc.Spec.Topology.Workers.NodePools
	if len(pools) != 2 || pools[1].Name != "workers-abcd1234" || pools[1].Replicas != 3 {
		t.Errorf("pools after add = %+v, want the clone appended last", pools)
	}
}

func TestPoolEditApplyDeleteRemovesByName(t *testing.T) {
	// The stored index says 0, but the list has been reordered since the
	// resolve: apply must find the pool by name, not trust the index.
	kc := scaleTestCluster(3, pool("workers", 12), pool("gpu", 2))
	edit := poolEdit{kind: poolEditDelete, poolName: "gpu", poolIndex: 0}
	if err := edit.apply(kc); err != nil {
		t.Fatalf("apply: %v", err)
	}
	pools := kc.Spec.Topology.Workers.NodePools
	if len(pools) != 1 || pools[0].Name != "workers" {
		t.Errorf("pools after delete = %+v, want only workers left", pools)
	}
}

func TestPoolEditApplyDeleteRefusesAVanishedPool(t *testing.T) {
	kc := scaleTestCluster(3, pool("workers", 12))
	edit := poolEdit{kind: poolEditDelete, poolName: "gpu", poolIndex: 1}
	if err := edit.apply(kc); err == nil {
		t.Error("deleting a pool that no longer exists must fail, not remove something else")
	}
}

func TestVerifyPoolSetUnchangedCatchesANameCollision(t *testing.T) {
	fresh := scaleTestCluster(3, pool("workers", 12), pool("workers-abcd1234", 1))
	edit := poolEdit{kind: poolEditAdd, newPool: pool("workers-abcd1234", 3)}
	if err := verifyPoolSetUnchanged(fresh, edit); err == nil {
		t.Error("an add whose name now exists must abort — appending would leave two pools with one name")
	}
}

func TestVerifyPoolSetUnchangedGuardsTheDelete(t *testing.T) {
	edit := poolEdit{kind: poolEditDelete, poolName: "gpu", poolIndex: 1}

	fresh := scaleTestCluster(3, pool("workers", 12), pool("gpu", 2))
	if err := verifyPoolSetUnchanged(fresh, edit); err != nil {
		t.Errorf("an untouched pool set must pass, got: %v", err)
	}

	if err := verifyPoolSetUnchanged(scaleTestCluster(3, pool("workers", 12)), edit); err == nil {
		t.Error("a delete whose pool vanished must abort")
	}

	// Someone else deleted the other pool while the prompt was open: this
	// delete would now take out the last one.
	if err := verifyPoolSetUnchanged(scaleTestCluster(3, pool("gpu", 2)), edit); err == nil {
		t.Error("a delete that would remove the last remaining pool must abort")
	}
}

func TestPatchPoolEditRetriesThroughStatusChurn(t *testing.T) {
	c := &churningClient{live: scaleTestCluster(3, pool("workers", 12)), conflicts: 2}
	edit := poolEdit{kind: poolEditAdd, newPool: pool("workers-abcd1234", 3)}

	out, err := patchPoolEdit(context.Background(), c, ctrlclient.ObjectKeyFromObject(c.live), edit)
	if err != nil {
		t.Fatalf("a conflict from status churn must be retried, not surfaced: %v", err)
	}
	if got := len(out.Spec.Topology.Workers.NodePools); got != 2 {
		t.Fatalf("patched cluster has %d pools, want 2", got)
	}
	if c.patches != 3 {
		t.Errorf("saw %d patch attempts, want 3 (two conflicts, then success)", c.patches)
	}
}

func TestPatchPoolEditAbortsWhenTheVerifyFails(t *testing.T) {
	// The pool set genuinely moved: retrying would re-read the same state,
	// so the loop must abort without a single write.
	c := &churningClient{live: scaleTestCluster(3, pool("workers", 12), pool("workers-abcd1234", 1))}
	edit := poolEdit{kind: poolEditAdd, newPool: pool("workers-abcd1234", 3)}

	if _, err := patchPoolEdit(context.Background(), c, ctrlclient.ObjectKeyFromObject(c.live), edit); err == nil {
		t.Fatal("a failed verify must surface as an error")
	}
	if c.patches != 0 {
		t.Errorf("saw %d patch attempts, want 0 — a failed verify must never write", c.patches)
	}
}

func TestPatchPoolEditDeleteRemovesThePool(t *testing.T) {
	c := &churningClient{live: scaleTestCluster(3, pool("workers", 12), pool("gpu", 2))}
	edit := poolEdit{kind: poolEditDelete, poolName: "gpu", poolIndex: 1}

	out, err := patchPoolEdit(context.Background(), c, ctrlclient.ObjectKeyFromObject(c.live), edit)
	if err != nil {
		t.Fatalf("patchPoolEdit: %v", err)
	}
	pools := out.Spec.Topology.Workers.NodePools
	if len(pools) != 1 || pools[0].Name != "workers" {
		t.Errorf("pools after delete = %+v, want only workers left", pools)
	}
}

func TestConfirmPoolEditDeleteRequiresTheTypedName(t *testing.T) {
	edit := poolEdit{kind: poolEditDelete, poolName: "gpu"}

	c, _ := scaleConfirmCmd("gpu\n")
	if err := confirmPoolEdit(c, edit); err != nil {
		t.Errorf("the typed pool name must confirm the delete, got: %v", err)
	}
	for _, answer := range []string{"y\n", "\n", "gpo\n"} {
		c, _ := scaleConfirmCmd(answer)
		if err := confirmPoolEdit(c, edit); err == nil {
			t.Errorf("answer %q must not confirm a pool delete", strings.TrimSpace(answer))
		}
	}
}

func TestConfirmPoolEditAddIsAYesNo(t *testing.T) {
	edit := poolEdit{kind: poolEditAdd, newPool: pool("workers-abcd1234", 3)}

	c, _ := scaleConfirmCmd("y\n")
	if err := confirmPoolEdit(c, edit); err != nil {
		t.Errorf("y must confirm an add, got: %v", err)
	}
	c, _ = scaleConfirmCmd("n\n")
	if err := confirmPoolEdit(c, edit); err == nil {
		t.Error("n must abort an add")
	}
}
