package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

func TestParseCountReadsAbsoluteValues(t *testing.T) {
	got, err := parseCount("5")
	if err != nil {
		t.Fatalf("parseCount(\"5\") returned an error: %v", err)
	}
	if got.delta {
		t.Error("a bare number must be absolute, not a delta")
	}
	if got.value != 5 {
		t.Errorf("value = %d, want 5", got.value)
	}
}

func TestParseCountReadsDeltas(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"+2", 2},
		{"-1", -1},
		{"+10", 10},
	} {
		got, err := parseCount(tc.in)
		if err != nil {
			t.Fatalf("parseCount(%q) returned an error: %v", tc.in, err)
		}
		if !got.delta {
			t.Errorf("parseCount(%q) must be a delta", tc.in)
		}
		if got.value != tc.want {
			t.Errorf("parseCount(%q).value = %d, want %d", tc.in, got.value, tc.want)
		}
	}
}

func TestParseCountRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "  ", "five", "3.5", "+", "-", "1 2", "++1"} {
		if _, err := parseCount(in); err == nil {
			t.Errorf("parseCount(%q) must fail rather than silently scale to something unintended", in)
		}
	}
}

func TestResolveAppliesDeltaToCurrent(t *testing.T) {
	c, err := parseCount("+2")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.resolve(3)
	if err != nil {
		t.Fatalf("resolve returned an error: %v", err)
	}
	if got != 5 {
		t.Errorf("3 with +2 applied = %d, want 5", got)
	}
}

func TestResolveIgnoresCurrentForAbsolute(t *testing.T) {
	c, err := parseCount("5")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.resolve(3)
	if err != nil {
		t.Fatalf("resolve returned an error: %v", err)
	}
	if got != 5 {
		t.Errorf("absolute 5 from current 3 = %d, want 5", got)
	}
}

func TestResolveRefusesNegativeResults(t *testing.T) {
	c, err := parseCount("-5")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.resolve(3); err == nil {
		t.Error("3 with -5 applied is negative and must be refused, not clamped silently")
	}
}

func TestParseNodePoolFlagSplitsNameAndCount(t *testing.T) {
	name, c, err := parseNodePoolFlag("workers=+2")
	if err != nil {
		t.Fatalf("parseNodePoolFlag returned an error: %v", err)
	}
	if name != "workers" {
		t.Errorf("name = %q, want \"workers\"", name)
	}
	if !c.delta || c.value != 2 {
		t.Errorf("count = %+v, want a delta of 2", c)
	}
}

func TestParseNodePoolFlagRequiresBothHalves(t *testing.T) {
	for _, in := range []string{"workers", "=5", "workers=", "=", "", "workers=abc"} {
		if _, _, err := parseNodePoolFlag(in); err == nil {
			t.Errorf("parseNodePoolFlag(%q) must fail", in)
		}
	}
}

func TestParseNodePoolFlagAcceptsNamesWithDashes(t *testing.T) {
	name, c, err := parseNodePoolFlag("gpu-a100=3")
	if err != nil {
		t.Fatalf("parseNodePoolFlag returned an error: %v", err)
	}
	if name != "gpu-a100" || c.delta || c.value != 3 {
		t.Errorf("got name=%q count=%+v, want gpu-a100 absolute 3", name, c)
	}
}

// cluster builds a KubernetesCluster with the given control plane replica
// count and node pools, for the target-resolution tests.
func scaleTestCluster(cpReplicas int, pools ...vitiv1alpha1.KubernetesClusterNodePool) *vitiv1alpha1.KubernetesCluster {
	kc := &vitiv1alpha1.KubernetesCluster{}
	kc.Name = "my-cluster"
	kc.Namespace = "team-a"
	kc.Spec.Topology.ControlPlane.Replicas = cpReplicas
	kc.Spec.Topology.Workers.NodePools = pools
	return kc
}

func pool(name string, replicas int) vitiv1alpha1.KubernetesClusterNodePool {
	return vitiv1alpha1.KubernetesClusterNodePool{Name: name, Replicas: replicas}
}

func TestClusterTargetsListsControlPlaneFirstThenEveryPool(t *testing.T) {
	kc := scaleTestCluster(3, pool("workers", 5), pool("gpu", 2))
	targets := clusterTargets(kc)
	if len(targets) != 3 {
		t.Fatalf("got %d targets, want 3 (control plane + 2 pools)", len(targets))
	}
	if targets[0].kind != targetControlPlane || targets[0].current != 3 {
		t.Errorf("targets[0] = %+v, want the control plane at 3 replicas", targets[0])
	}
	if targets[1].name != "workers" || targets[1].current != 5 || targets[1].index != 0 {
		t.Errorf("targets[1] = %+v, want pool workers at 5 replicas, index 0", targets[1])
	}
	if targets[2].name != "gpu" || targets[2].current != 2 || targets[2].index != 1 {
		t.Errorf("targets[2] = %+v, want pool gpu at 2 replicas, index 1", targets[2])
	}
}

func TestClusterTargetsCarriesAutoscalingState(t *testing.T) {
	p := pool("workers", 5)
	p.Autoscaling.Enabled = true
	p.Autoscaling.MinReplicas = 3
	p.Autoscaling.MaxReplicas = 9
	targets := clusterTargets(scaleTestCluster(3, p))
	if !targets[1].autoscaling {
		t.Error("an autoscaling pool must be flagged so the operator is warned before fighting the autoscaler")
	}
}

func TestFindNodePoolTargetMatchesByName(t *testing.T) {
	targets := clusterTargets(scaleTestCluster(3, pool("workers", 5), pool("gpu", 2)))
	got, err := findNodePoolTarget(targets, "gpu")
	if err != nil {
		t.Fatalf("findNodePoolTarget returned an error: %v", err)
	}
	if got.name != "gpu" || got.current != 2 {
		t.Errorf("got %+v, want pool gpu at 2 replicas", got)
	}
}

func TestFindNodePoolTargetNamesTheAvailablePools(t *testing.T) {
	targets := clusterTargets(scaleTestCluster(3, pool("workers", 5), pool("gpu", 2)))
	_, err := findNodePoolTarget(targets, "nope")
	if err == nil {
		t.Fatal("an unknown pool name must fail")
	}
	for _, want := range []string{"nope", "workers", "gpu"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q so the operator can correct the name, got: %s", want, err)
		}
	}
}

func TestFindNodePoolTargetOnAClusterWithNoPools(t *testing.T) {
	targets := clusterTargets(scaleTestCluster(3))
	_, err := findNodePoolTarget(targets, "workers")
	if err == nil {
		t.Fatal("a cluster with no node pools must report that, not an empty name list")
	}
	if !strings.Contains(err.Error(), "no node pools") {
		t.Errorf("error should say the cluster has no node pools, got: %s", err)
	}
}

func TestValidateRefusesEvenControlPlaneCounts(t *testing.T) {
	ch := scaleChange{target: scaleTarget{kind: targetControlPlane, current: 3}, desired: 4}
	err := ch.validate()
	if err == nil {
		t.Fatal("an even control plane count breaks etcd quorum and must be refused locally")
	}
	for _, want := range []string{"odd", "quorum"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("error should explain the etcd quorum reason (%q), got: %s", want, err)
		}
	}
}

func TestValidateAcceptsOddControlPlaneCounts(t *testing.T) {
	for _, n := range []int{1, 3, 5} {
		ch := scaleChange{target: scaleTarget{kind: targetControlPlane, current: 3}, desired: n}
		if err := ch.validate(); err != nil {
			t.Errorf("control plane count %d must be accepted, got: %v", n, err)
		}
	}
}

func TestValidateRefusesZeroControlPlane(t *testing.T) {
	ch := scaleChange{target: scaleTarget{kind: targetControlPlane, current: 3}, desired: 0}
	if err := ch.validate(); err == nil {
		t.Fatal("scaling the control plane to zero destroys the cluster and must be refused")
	}
}

func TestValidateAllowsZeroNodePool(t *testing.T) {
	ch := scaleChange{target: scaleTarget{kind: targetNodePool, name: "workers", current: 3}, desired: 0}
	if err := ch.validate(); err != nil {
		t.Errorf("draining a node pool to zero is legitimate, got: %v", err)
	}
}

func TestChangeIsNoopWhenDesiredMatchesCurrent(t *testing.T) {
	ch := scaleChange{target: scaleTarget{kind: targetNodePool, name: "workers", current: 3}, desired: 3}
	if !ch.noop() {
		t.Error("scaling to the current value must be recognised as a no-op so no patch is sent")
	}
	ch.desired = 4
	if ch.noop() {
		t.Error("a real change must not be reported as a no-op")
	}
}

func TestDescribeAlwaysNamesTheNodePool(t *testing.T) {
	// Even on a cluster with a single pool, where nothing was ambiguous, the
	// output must say which pool it changed.
	ch := scaleChange{target: scaleTarget{kind: targetNodePool, name: "workers", current: 3}, desired: 5}
	got := ch.describe()
	for _, want := range []string{"workers", "3", "5"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe() = %q, must contain %q", got, want)
		}
	}
}

func TestDescribeNamesTheControlPlane(t *testing.T) {
	ch := scaleChange{target: scaleTarget{kind: targetControlPlane, current: 3}, desired: 5}
	got := ch.describe()
	if !strings.Contains(strings.ToLower(got), "control plane") {
		t.Errorf("describe() = %q, must name the control plane", got)
	}
	for _, want := range []string{"3", "5"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe() = %q, must contain %q", got, want)
		}
	}
}

func TestIsScaleDownComparesAgainstCurrent(t *testing.T) {
	up := scaleChange{target: scaleTarget{kind: targetControlPlane, current: 3}, desired: 5}
	down := scaleChange{target: scaleTarget{kind: targetControlPlane, current: 5}, desired: 3}
	if up.scalingDown() {
		t.Error("3 -> 5 is not a scale-down")
	}
	if !down.scalingDown() {
		t.Error("5 -> 3 is a scale-down and must trigger the stronger confirmation")
	}
}

func TestResolveChangesReadsTheControlPlaneFlag(t *testing.T) {
	kc := scaleTestCluster(3, pool("workers", 5))
	got, err := resolveChanges(kc, "+2", nil)
	if err != nil {
		t.Fatalf("resolveChanges returned an error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d changes, want 1", len(got))
	}
	if got[0].target.kind != targetControlPlane || got[0].desired != 5 {
		t.Errorf("got %+v, want the control plane at 5", got[0])
	}
}

func TestResolveChangesReadsNodePoolFlagsAgainstCurrentReplicas(t *testing.T) {
	kc := scaleTestCluster(3, pool("workers", 5), pool("gpu", 2))
	got, err := resolveChanges(kc, "", []string{"gpu=+3"})
	if err != nil {
		t.Fatalf("resolveChanges returned an error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d changes, want 1", len(got))
	}
	if got[0].target.name != "gpu" || got[0].desired != 5 {
		t.Errorf("got %+v, want pool gpu at 5 (2 + 3)", got[0])
	}
}

func TestResolveChangesCombinesControlPlaneAndSeveralPools(t *testing.T) {
	kc := scaleTestCluster(1, pool("workers", 5), pool("gpu", 2))
	got, err := resolveChanges(kc, "3", []string{"workers=6", "gpu=-1"})
	if err != nil {
		t.Fatalf("resolveChanges returned an error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d changes, want 3", len(got))
	}
	want := map[string]int{"control plane": 3, "node pool \"workers\"": 6, "node pool \"gpu\"": 1}
	for _, ch := range got {
		w, ok := want[ch.target.label()]
		if !ok {
			t.Errorf("unexpected change for %s", ch.target.label())
			continue
		}
		if ch.desired != w {
			t.Errorf("%s desired = %d, want %d", ch.target.label(), ch.desired, w)
		}
	}
}

func TestResolveChangesRefusesTheSamePoolTwice(t *testing.T) {
	kc := scaleTestCluster(3, pool("workers", 5))
	_, err := resolveChanges(kc, "", []string{"workers=6", "workers=8"})
	if err == nil {
		t.Fatal("two counts for one pool is ambiguous and must be refused, not resolved last-wins")
	}
	if !strings.Contains(err.Error(), "workers") {
		t.Errorf("error should name the duplicated pool, got: %s", err)
	}
}

func TestResolveChangesRejectsAnUnknownPool(t *testing.T) {
	kc := scaleTestCluster(3, pool("workers", 5))
	if _, err := resolveChanges(kc, "", []string{"nope=6"}); err == nil {
		t.Fatal("an unknown pool must fail")
	}
}

func TestResolveChangesValidatesEachChange(t *testing.T) {
	kc := scaleTestCluster(3, pool("workers", 5))
	_, err := resolveChanges(kc, "4", nil)
	if err == nil {
		t.Fatal("an even control plane count must be refused by resolveChanges, not left to the apiserver")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "odd") {
		t.Errorf("error should explain the odd-replica rule, got: %s", err)
	}
}

func TestResolveChangesRefusesToScaleAPoolBelowZero(t *testing.T) {
	kc := scaleTestCluster(3, pool("workers", 2))
	if _, err := resolveChanges(kc, "", []string{"workers=-5"}); err == nil {
		t.Fatal("2 with -5 applied is negative and must be refused")
	}
}

func TestResolveChangesWithNoFlagsReturnsNothing(t *testing.T) {
	kc := scaleTestCluster(3, pool("workers", 5))
	got, err := resolveChanges(kc, "", nil)
	if err != nil {
		t.Fatalf("resolveChanges returned an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d changes with no flags set, want 0 (the caller falls back to the picker)", len(got))
	}
}

func TestApplyChangesWritesTheControlPlaneReplicas(t *testing.T) {
	kc := scaleTestCluster(3, pool("workers", 5))
	changes := []scaleChange{{target: scaleTarget{kind: targetControlPlane, index: -1, current: 3}, desired: 5}}
	if err := applyChanges(kc, changes); err != nil {
		t.Fatalf("applyChanges returned an error: %v", err)
	}
	if kc.Spec.Topology.ControlPlane.Replicas != 5 {
		t.Errorf("control plane replicas = %d, want 5", kc.Spec.Topology.ControlPlane.Replicas)
	}
	if kc.Spec.Topology.Workers.NodePools[0].Replicas != 5 {
		t.Error("scaling the control plane must not touch a node pool")
	}
}

func TestApplyChangesWritesOnlyTheNamedPool(t *testing.T) {
	kc := scaleTestCluster(3, pool("workers", 5), pool("gpu", 2))
	changes := []scaleChange{{target: scaleTarget{kind: targetNodePool, name: "gpu", index: 1, current: 2}, desired: 4}}
	if err := applyChanges(kc, changes); err != nil {
		t.Fatalf("applyChanges returned an error: %v", err)
	}
	if got := kc.Spec.Topology.Workers.NodePools[1].Replicas; got != 4 {
		t.Errorf("gpu replicas = %d, want 4", got)
	}
	if got := kc.Spec.Topology.Workers.NodePools[0].Replicas; got != 5 {
		t.Errorf("workers replicas = %d, want 5 (untouched)", got)
	}
	if kc.Spec.Topology.ControlPlane.Replicas != 3 {
		t.Error("scaling a node pool must not touch the control plane")
	}
}

func TestApplyChangesRefusesAPoolThatMovedSinceItWasResolved(t *testing.T) {
	// The index is resolved against the spec that was read; if the list no
	// longer holds that pool at that index, writing by index would scale the
	// wrong pool.
	kc := scaleTestCluster(3, pool("workers", 5))
	changes := []scaleChange{{target: scaleTarget{kind: targetNodePool, name: "gpu", index: 0, current: 2}, desired: 4}}
	if err := applyChanges(kc, changes); err == nil {
		t.Fatal("a pool whose name no longer matches its index must be refused, not written blindly")
	}
}

func TestApplyChangesRefusesAnIndexOutOfRange(t *testing.T) {
	kc := scaleTestCluster(3, pool("workers", 5))
	changes := []scaleChange{{target: scaleTarget{kind: targetNodePool, name: "gpu", index: 7, current: 2}, desired: 4}}
	if err := applyChanges(kc, changes); err == nil {
		t.Fatal("an out-of-range pool index must be refused rather than panicking")
	}
}

func TestPartitionChangesSeparatesNoops(t *testing.T) {
	changes := []scaleChange{
		{target: scaleTarget{kind: targetNodePool, name: "workers", current: 3}, desired: 5},
		{target: scaleTarget{kind: targetNodePool, name: "gpu", current: 2}, desired: 2},
	}
	actionable, noops := partitionChanges(changes)
	if len(actionable) != 1 || actionable[0].target.name != "workers" {
		t.Errorf("actionable = %+v, want just workers", actionable)
	}
	if len(noops) != 1 || noops[0].target.name != "gpu" {
		t.Errorf("noops = %+v, want just gpu", noops)
	}
}

// planFor renders the plan a run would print, for assertions on its wording.
func planFor(t *testing.T, changes ...scaleChange) string {
	t.Helper()
	var sb strings.Builder
	actionable, noops := partitionChanges(changes)
	writeScalePlan(&sb, "az1", "team-a", "my-cluster", actionable, noops)
	return sb.String()
}

func TestPlanNamesTheNodePoolEvenWhenTheClusterHasOnlyOne(t *testing.T) {
	// The whole point: with a single pool nothing was ambiguous and no picker
	// was shown, but the operator must still be told which pool changed.
	got := planFor(t, scaleChange{
		target:  scaleTarget{kind: targetNodePool, name: "workers", index: 0, current: 3},
		desired: 5,
	})
	if !strings.Contains(got, "workers") {
		t.Errorf("plan must name the pool it changes even on a single-pool cluster, got:\n%s", got)
	}
	for _, want := range []string{"my-cluster", "team-a", "az1", "3", "5"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan must contain %q, got:\n%s", want, got)
		}
	}
}

func TestPlanNamesEveryPoolWhenSeveralChange(t *testing.T) {
	got := planFor(t,
		scaleChange{target: scaleTarget{kind: targetNodePool, name: "workers", index: 0, current: 3}, desired: 5},
		scaleChange{target: scaleTarget{kind: targetNodePool, name: "gpu", index: 1, current: 2}, desired: 4},
	)
	for _, want := range []string{"workers", "gpu"} {
		if !strings.Contains(got, want) {
			t.Errorf("plan must name %q, got:\n%s", want, got)
		}
	}
}

func TestPlanReportsNoopsWithoutPresentingThemAsChanges(t *testing.T) {
	got := planFor(t, scaleChange{
		target:  scaleTarget{kind: targetNodePool, name: "workers", index: 0, current: 3},
		desired: 3,
	})
	if !strings.Contains(got, "workers") {
		t.Errorf("a no-op must still name the pool, got:\n%s", got)
	}
	if !strings.Contains(got, "already") {
		t.Errorf("a no-op must be reported as already at that count, got:\n%s", got)
	}
	if strings.Contains(got, "->") {
		t.Errorf("a no-op must not render as a transition, got:\n%s", got)
	}
}

func TestPlanWarnsAboutAutoscalingPools(t *testing.T) {
	got := planFor(t, scaleChange{
		target:  scaleTarget{kind: targetNodePool, name: "workers", index: 0, current: 3, autoscaling: true},
		desired: 5,
	})
	if !strings.Contains(strings.ToLower(got), "autoscal") {
		t.Errorf("scaling an autoscaling pool must warn that the autoscaler will fight the value, got:\n%s", got)
	}
	if !strings.Contains(got, "workers") {
		t.Errorf("the autoscaling warning must name the pool, got:\n%s", got)
	}
}

func TestPlanDoesNotWarnAboutAutoscalingWhenDisabled(t *testing.T) {
	got := planFor(t, scaleChange{
		target:  scaleTarget{kind: targetNodePool, name: "workers", index: 0, current: 3},
		desired: 5,
	})
	if strings.Contains(strings.ToLower(got), "autoscal") {
		t.Errorf("a pool without autoscaling must not carry the warning, got:\n%s", got)
	}
}

func TestChangesIncludeAControlPlaneScaleDown(t *testing.T) {
	changes := []scaleChange{
		{target: scaleTarget{kind: targetNodePool, name: "workers", current: 3}, desired: 5},
		{target: scaleTarget{kind: targetControlPlane, current: 5}, desired: 3},
	}
	if !hasControlPlaneScaleDown(changes) {
		t.Error("removing control plane nodes must be detected so the stronger confirmation is demanded")
	}
	if hasControlPlaneScaleDown(changes[:1]) {
		t.Error("a worker-only change must not demand the control plane confirmation")
	}
}

func TestControlPlaneScaleUpIsNotAScaleDown(t *testing.T) {
	changes := []scaleChange{{target: scaleTarget{kind: targetControlPlane, current: 3}, desired: 5}}
	if hasControlPlaneScaleDown(changes) {
		t.Error("adding control plane nodes is not a scale-down")
	}
}

// resetScaleFlags restores the command's flag state between parsing tests.
func resetScaleFlags(t *testing.T) {
	t.Helper()
	kcScaleControlPlane = ""
	kcScaleNodePools = nil
	kcScaleNodePoolsAlias = nil
	// Clearing Changed is what lets the next ParseFlags start from scratch:
	// a string-array flag only resets its target on its first Set.
	for _, name := range []string{"controlplane", "cp", "nodepool", "np"} {
		if f := kcScaleCmd.Flags().Lookup(name); f != nil {
			f.Changed = false
		}
	}
}

func TestNodePoolAliasAccumulatesWithTheLongFlag(t *testing.T) {
	resetScaleFlags(t)
	if err := kcScaleCmd.ParseFlags([]string{"--nodepool", "a=1", "--np", "b=2"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got := scaleNodePoolFlags()
	if len(got) != 2 {
		t.Fatalf("--nodepool a=1 --np b=2 collected %v, want both counts — an alias must not drop the other flag's values", got)
	}
}

func TestNodePoolLongFlagRepeats(t *testing.T) {
	resetScaleFlags(t)
	if err := kcScaleCmd.ParseFlags([]string{"--nodepool", "a=1", "--nodepool", "b=2"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if got := scaleNodePoolFlags(); len(got) != 2 {
		t.Fatalf("repeated --nodepool collected %v, want 2", got)
	}
}

func TestNodePoolAliasAloneIsCollected(t *testing.T) {
	resetScaleFlags(t)
	if err := kcScaleCmd.ParseFlags([]string{"--np", "a=1"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if got := scaleNodePoolFlags(); len(got) != 1 || got[0] != "a=1" {
		t.Fatalf("--np alone collected %v, want [a=1]", got)
	}
}

func TestControlPlaneAliasIsAccepted(t *testing.T) {
	resetScaleFlags(t)
	if err := kcScaleCmd.ParseFlags([]string{"--cp", "3"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if kcScaleControlPlane != "3" {
		t.Errorf("--cp 3 set %q, want \"3\"", kcScaleControlPlane)
	}
	resetScaleFlags(t)
	if err := kcScaleCmd.ParseFlags([]string{"--controlplane", "5"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if kcScaleControlPlane != "5" {
		t.Errorf("--controlplane 5 set %q, want \"5\"", kcScaleControlPlane)
	}
}

func TestCheckScaleFlagsCatchesSyntaxBeforeAnyLookup(t *testing.T) {
	// Flag syntax needs no cluster, so it must be rejected before the command
	// connects to every availability zone — otherwise a typo costs a fleet
	// round trip and its error is buried under connection warnings.
	for _, tc := range []struct {
		name    string
		cpFlag  string
		npFlags []string
	}{
		{"unparseable control plane count", "abc", nil},
		{"unparseable pool count", "", []string{"workers=abc"}},
		{"pool spec without a count", "", []string{"workers"}},
		{"pool spec without a name", "", []string{"=5"}},
		{"the same pool twice", "", []string{"workers=1", "workers=2"}},
	} {
		if err := checkScaleFlags(tc.cpFlag, tc.npFlags); err == nil {
			t.Errorf("%s must be caught before any lookup", tc.name)
		}
	}
}

func TestCheckScaleFlagsAcceptsValidSyntax(t *testing.T) {
	for _, tc := range []struct {
		cpFlag  string
		npFlags []string
	}{
		{"", nil},
		{"3", nil},
		{"+2", []string{"workers=5", "gpu=-1"}},
		{"", []string{"workers=+2"}},
	} {
		if err := checkScaleFlags(tc.cpFlag, tc.npFlags); err != nil {
			t.Errorf("checkScaleFlags(%q, %v) rejected valid syntax: %v", tc.cpFlag, tc.npFlags, err)
		}
	}
}

func TestCheckScaleFlagsDoesNotJudgeCountsItCannotYetResolve(t *testing.T) {
	// An even control plane count is only wrong once resolved against the
	// cluster: "+1" from 2 is odd. Syntax checking must not pre-empt that.
	if err := checkScaleFlags("+1", nil); err != nil {
		t.Errorf("a relative count cannot be judged without the cluster, got: %v", err)
	}
}

// scaleConfirmCmd builds a command whose stdin is the given answer, so the
// confirmation paths can be driven without a terminal.
func scaleConfirmCmd(answer string) (*cobra.Command, *bytes.Buffer) {
	c := &cobra.Command{}
	out := &bytes.Buffer{}
	c.SetOut(out)
	c.SetIn(strings.NewReader(answer))
	return c, out
}

func TestConfirmScaleAcceptsYesForAWorkerChange(t *testing.T) {
	c, _ := scaleConfirmCmd("y\n")
	changes := []scaleChange{{target: scaleTarget{kind: targetNodePool, name: "workers", current: 3}, desired: 5}}
	if err := confirmScale(c, "my-cluster", changes); err != nil {
		t.Errorf("a y answer must proceed, got: %v", err)
	}
}

func TestConfirmScaleAbortsOnAnythingButYes(t *testing.T) {
	for _, answer := range []string{"n\n", "\n", "no\n", "maybe\n"} {
		c, _ := scaleConfirmCmd(answer)
		changes := []scaleChange{{target: scaleTarget{kind: targetNodePool, name: "workers", current: 3}, desired: 5}}
		if err := confirmScale(c, "my-cluster", changes); err == nil {
			t.Errorf("answer %q must abort the scale, not proceed", answer)
		}
	}
}

func TestConfirmScaleDemandsTheClusterNameForAControlPlaneScaleDown(t *testing.T) {
	// A bare "y" must not be enough to remove etcd members.
	c, _ := scaleConfirmCmd("y\n")
	changes := []scaleChange{{target: scaleTarget{kind: targetControlPlane, current: 5}, desired: 3}}
	if err := confirmScale(c, "my-cluster", changes); err == nil {
		t.Fatal("removing control plane nodes must require the cluster name, not a y/N")
	}

	c, _ = scaleConfirmCmd("my-cluster\n")
	if err := confirmScale(c, "my-cluster", changes); err != nil {
		t.Errorf("typing the cluster name must proceed, got: %v", err)
	}
}

func TestConfirmScaleRejectsTheWrongClusterName(t *testing.T) {
	c, _ := scaleConfirmCmd("other-cluster\n")
	changes := []scaleChange{{target: scaleTarget{kind: targetControlPlane, current: 5}, desired: 3}}
	if err := confirmScale(c, "my-cluster", changes); err == nil {
		t.Fatal("a mismatched name must abort")
	}
}

func TestConfirmScaleUsesTheWeakerPromptForAControlPlaneScaleUp(t *testing.T) {
	// Adding control plane nodes does not shrink etcd membership, so it is a
	// y/N like any other change.
	c, _ := scaleConfirmCmd("y\n")
	changes := []scaleChange{{target: scaleTarget{kind: targetControlPlane, current: 1}, desired: 3}}
	if err := confirmScale(c, "my-cluster", changes); err != nil {
		t.Errorf("a control plane scale-up must accept y, got: %v", err)
	}
}

func TestPromptCountNamesTheTargetAndItsCurrentValue(t *testing.T) {
	c, out := scaleConfirmCmd("+2\n")
	got, err := promptCount(c, scaleTarget{kind: targetNodePool, name: "wp", current: 2})
	if err != nil {
		t.Fatalf("promptCount returned an error: %v", err)
	}
	if !got.delta || got.value != 2 {
		t.Errorf("promptCount read %+v, want a delta of 2", got)
	}
	for _, want := range []string{"wp", "2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the prompt must show %q so the answer is given with the starting point in view, got: %q",
				want, out.String())
		}
	}
}

func TestPromptCountRejectsAnUnreadableAnswer(t *testing.T) {
	c, _ := scaleConfirmCmd("lots\n")
	if _, err := promptCount(c, scaleTarget{kind: targetNodePool, name: "wp", current: 2}); err == nil {
		t.Error("an unreadable answer must fail rather than resolve to something unintended")
	}
}

func TestVerifyTargetsUnchangedAcceptsAnUntouchedSpec(t *testing.T) {
	fresh := scaleTestCluster(3, pool("wp", 3))
	changes := []scaleChange{{target: scaleTarget{kind: targetNodePool, name: "wp", index: 0, current: 3}, desired: 4}}
	if err := verifyTargetsUnchanged(fresh, changes); err != nil {
		t.Errorf("an untouched spec must be accepted, got: %v", err)
	}
}

func TestVerifyTargetsUnchangedRejectsAPoolThatMovedUnderneath(t *testing.T) {
	// The operator rewrites status constantly, which is harmless. A change to
	// the replica count we showed the user is not: "+1 from 3" was approved as
	// 3 -> 4, and must not silently land on a pool that is now at 6.
	fresh := scaleTestCluster(3, pool("wp", 6))
	changes := []scaleChange{{target: scaleTarget{kind: targetNodePool, name: "wp", index: 0, current: 3}, desired: 4}}
	err := verifyTargetsUnchanged(fresh, changes)
	if err == nil {
		t.Fatal("a pool whose replica count moved must abort the write")
	}
	for _, want := range []string{"wp", "3", "6"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name the pool and both counts (%q), got: %s", want, err)
		}
	}
}

func TestVerifyTargetsUnchangedRejectsAControlPlaneThatMovedUnderneath(t *testing.T) {
	fresh := scaleTestCluster(5, pool("wp", 3))
	changes := []scaleChange{{target: scaleTarget{kind: targetControlPlane, index: -1, current: 3}, desired: 5}}
	if err := verifyTargetsUnchanged(fresh, changes); err == nil {
		t.Fatal("a control plane whose replica count moved must abort the write")
	}
}

func TestVerifyTargetsUnchangedIgnoresPoolsTheChangeDoesNotTouch(t *testing.T) {
	// Someone scaling a different pool must not block this write — that is
	// exactly the concurrent edit a fresh-read patch preserves.
	fresh := scaleTestCluster(3, pool("wp", 3), pool("gpu", 99))
	changes := []scaleChange{{target: scaleTarget{kind: targetNodePool, name: "wp", index: 0, current: 3}, desired: 4}}
	if err := verifyTargetsUnchanged(fresh, changes); err != nil {
		t.Errorf("a change to an untouched pool must not block the write, got: %v", err)
	}
}

func TestVerifyTargetsUnchangedRejectsAPoolThatDisappeared(t *testing.T) {
	fresh := scaleTestCluster(3, pool("other", 3))
	changes := []scaleChange{{target: scaleTarget{kind: targetNodePool, name: "wp", index: 0, current: 3}, desired: 4}}
	if err := verifyTargetsUnchanged(fresh, changes); err == nil {
		t.Fatal("a pool that no longer exists must abort the write")
	}
}

// churningClient stands in for a cluster whose operator rewrites status
// between the read and the write — the condition that made every interactive
// run fail. Each Patch call before conflicts is rejected the way the
// apiserver rejects a stale resourceVersion.
type churningClient struct {
	ctrlclient.Client
	live      *vitiv1alpha1.KubernetesCluster
	conflicts int
	gets      int
	patches   int
}

func (c *churningClient) Get(_ context.Context, _ ctrlclient.ObjectKey, obj ctrlclient.Object, _ ...ctrlclient.GetOption) error {
	kc, ok := obj.(*vitiv1alpha1.KubernetesCluster)
	if !ok {
		return fmt.Errorf("unexpected object %T", obj)
	}
	c.gets++
	c.live.DeepCopyInto(kc)
	return nil
}

func (c *churningClient) Patch(_ context.Context, obj ctrlclient.Object, _ ctrlclient.Patch, _ ...ctrlclient.PatchOption) error {
	c.patches++
	if c.patches <= c.conflicts {
		return apierrors.NewConflict(
			schema.GroupResource{Group: "vitistack.io", Resource: "kubernetesclusters"},
			c.live.Name, errors.New("the object has been modified; please apply your changes to the latest version and try again"))
	}
	kc, ok := obj.(*vitiv1alpha1.KubernetesCluster)
	if !ok {
		return fmt.Errorf("unexpected object %T", obj)
	}
	kc.DeepCopyInto(c.live)
	return nil
}

func TestPatchScaleRetriesThroughStatusChurn(t *testing.T) {
	// The bug: one conflict aborted the whole command. Status churn is
	// routine, so the write must re-read and try again rather than fail.
	c := &churningClient{live: scaleTestCluster(3, pool("wp", 3)), conflicts: 2}
	changes := []scaleChange{{target: scaleTarget{kind: targetNodePool, name: "wp", index: 0, current: 3}, desired: 4}}

	if _, err := patchScale(context.Background(), c, ctrlclient.ObjectKeyFromObject(c.live), changes); err != nil {
		t.Fatalf("a conflict from status churn must be retried, not surfaced: %v", err)
	}
	if got := c.live.Spec.Topology.Workers.NodePools[0].Replicas; got != 4 {
		t.Errorf("wp replicas = %d, want 4", got)
	}
	if c.gets < 2 {
		t.Errorf("got %d Gets — each retry must re-read the object, not reuse the stale one", c.gets)
	}
}

func TestPatchScaleSucceedsFirstTimeWhenNothingConflicts(t *testing.T) {
	c := &churningClient{live: scaleTestCluster(3, pool("wp", 3))}
	changes := []scaleChange{{target: scaleTarget{kind: targetNodePool, name: "wp", index: 0, current: 3}, desired: 4}}
	if _, err := patchScale(context.Background(), c, ctrlclient.ObjectKeyFromObject(c.live), changes); err != nil {
		t.Fatalf("patchScale returned an error: %v", err)
	}
	if c.patches != 1 {
		t.Errorf("got %d patches, want 1", c.patches)
	}
}

func TestPatchScaleAbortsWhenTheTargetItselfMoved(t *testing.T) {
	// Retrying must not paper over a real spec change: the count the user
	// approved was relative to 3, and the pool is now at 6.
	c := &churningClient{live: scaleTestCluster(3, pool("wp", 6))}
	changes := []scaleChange{{target: scaleTarget{kind: targetNodePool, name: "wp", index: 0, current: 3}, desired: 4}}

	_, err := patchScale(context.Background(), c, ctrlclient.ObjectKeyFromObject(c.live), changes)
	if err == nil {
		t.Fatal("a target that moved underneath must abort")
	}
	if c.patches != 0 {
		t.Errorf("got %d patches — nothing must be written when the target moved", c.patches)
	}
	if c.live.Spec.Topology.Workers.NodePools[0].Replicas != 6 {
		t.Error("the live object must be left exactly as it was")
	}
}

func TestPatchScaleGivesUpAfterPersistentConflicts(t *testing.T) {
	// A cluster that conflicts forever must end in an error, not a hang.
	c := &churningClient{live: scaleTestCluster(3, pool("wp", 3)), conflicts: 1000}
	changes := []scaleChange{{target: scaleTarget{kind: targetNodePool, name: "wp", index: 0, current: 3}, desired: 4}}
	if _, err := patchScale(context.Background(), c, ctrlclient.ObjectKeyFromObject(c.live), changes); err == nil {
		t.Fatal("persistent conflicts must surface as an error")
	}
}
