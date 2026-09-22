package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/pkg/plugin/picker"
)

// countSpec is a parsed replica count. It is either an absolute target
// ("5") or a delta to apply to whatever the cluster currently declares
// ("+2", "-1"). Deltas exist so "one more worker" needs no arithmetic;
// absolutes exist so a scripted invocation stays idempotent.
type countSpec struct {
	delta bool
	value int
}

// parseCount reads a count in either form. It is deliberately strict:
// anything it cannot read exactly is an error, because a misread count
// silently scales a production cluster to the wrong size.
func parseCount(s string) (countSpec, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return countSpec{}, fmt.Errorf("empty replica count: want a number (5), or a delta (+2, -1)")
	}
	delta := strings.HasPrefix(trimmed, "+") || strings.HasPrefix(trimmed, "-")
	// strconv.Atoi accepts a leading sign, which is exactly the delta form;
	// for the absolute form the sign is what distinguishes the two.
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return countSpec{}, fmt.Errorf("%q is not a replica count: want a number (5), or a delta (+2, -1)", s)
	}
	return countSpec{delta: delta, value: n}, nil
}

// resolve turns the spec into the replica count to write, given what the
// cluster declares today.
func (c countSpec) resolve(current int) (int, error) {
	if !c.delta {
		if c.value < 0 {
			return 0, fmt.Errorf("replica count %d is negative", c.value)
		}
		return c.value, nil
	}
	next := current + c.value
	if next < 0 {
		return 0, fmt.Errorf("%d%+d is %d — cannot scale below zero", current, c.value, next)
	}
	return next, nil
}

// parseNodePoolFlag splits a --nodepool value ("workers=+2") into the pool
// name and its count.
func parseNodePoolFlag(s string) (string, countSpec, error) {
	name, count, ok := strings.Cut(s, "=")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return "", countSpec{}, fmt.Errorf(
			"%q is not a node pool count: want <pool>=<count>, e.g. workers=5, workers=+2, workers=-1", s)
	}
	c, err := parseCount(count)
	if err != nil {
		return "", countSpec{}, fmt.Errorf("node pool %q: %w", name, err)
	}
	return name, c, nil
}

// targetKind distinguishes the two things in a cluster that have a replica
// count of their own.
type targetKind int

const (
	targetControlPlane targetKind = iota
	targetNodePool
)

// scaleTarget is one scalable part of a cluster, resolved against the
// cluster's current spec.
type scaleTarget struct {
	kind targetKind
	// name is the node pool's name; empty for the control plane.
	name string
	// index is the position in Spec.Topology.Workers.NodePools, or -1 for
	// the control plane.
	index       int
	current     int
	autoscaling bool
	// machineClass is carried for display only, so the picker row and the
	// confirmation say which pool in terms the operator recognises.
	machineClass string
}

// label names the target the way every message about it should: a node pool
// is always identified by name, even on a cluster that has only one, so the
// output never leaves the operator guessing what was changed.
func (t scaleTarget) label() string {
	if t.kind == targetControlPlane {
		return "control plane"
	}
	name := t.name
	if name == "" {
		name = fmt.Sprintf("<unnamed pool #%d>", t.index)
	}
	return fmt.Sprintf("node pool %q", name)
}

// clusterTargets lists everything in the cluster that can be scaled: the
// control plane first, then each node pool in spec order.
func clusterTargets(kc *vitiv1alpha1.KubernetesCluster) []scaleTarget {
	pools := kc.Spec.Topology.Workers.NodePools
	targets := make([]scaleTarget, 0, len(pools)+1)
	targets = append(targets, scaleTarget{
		kind:         targetControlPlane,
		index:        -1,
		current:      kc.Spec.Topology.ControlPlane.Replicas,
		machineClass: kc.Spec.Topology.ControlPlane.MachineClass,
	})
	for i := range pools {
		targets = append(targets, scaleTarget{
			kind:         targetNodePool,
			name:         pools[i].Name,
			index:        i,
			current:      pools[i].Replicas,
			autoscaling:  pools[i].Autoscaling.Enabled,
			machineClass: pools[i].MachineClass,
		})
	}
	return targets
}

// nodePoolTargets returns only the node pools.
func nodePoolTargets(targets []scaleTarget) []scaleTarget {
	out := make([]scaleTarget, 0, len(targets))
	for _, t := range targets {
		if t.kind == targetNodePool {
			out = append(out, t)
		}
	}
	return out
}

// findNodePoolTarget resolves a --nodepool name against the cluster. A miss
// lists what the cluster does have, so a typo is one correction away rather
// than a second lookup.
func findNodePoolTarget(targets []scaleTarget, name string) (scaleTarget, error) {
	pools := nodePoolTargets(targets)
	if len(pools) == 0 {
		return scaleTarget{}, fmt.Errorf("this cluster declares no node pools — there is nothing to scale but the control plane")
	}
	names := make([]string, 0, len(pools))
	for _, p := range pools {
		if p.name == name {
			return p, nil
		}
		names = append(names, strconv.Quote(p.name))
	}
	return scaleTarget{}, fmt.Errorf("no node pool named %q on this cluster — it has %s",
		name, strings.Join(names, ", "))
}

// scaleChange is a resolved target plus the count to write to it.
type scaleChange struct {
	target  scaleTarget
	desired int
}

// noop reports whether the change would write the value already there.
// Scaling to the current count must not produce a patch: at fleet scale a
// no-op write is still apiserver churn and a spurious resource version.
func (ch scaleChange) noop() bool { return ch.desired == ch.target.current }

// scalingDown reports whether the change removes nodes. Callers use it to
// decide how strong a confirmation to demand.
func (ch scaleChange) scalingDown() bool { return ch.desired < ch.target.current }

// describe renders the change as one line. It always names the target — a
// node pool by name even when the cluster has only one.
func (ch scaleChange) describe() string {
	return fmt.Sprintf("%s: %d -> %d replicas", ch.target.label(), ch.target.current, ch.desired)
}

// validate applies the rules the CRD would apply, before the round trip, so
// a rejection arrives with the reason rather than as a CEL message.
func (ch scaleChange) validate() error {
	if ch.desired < 0 {
		return fmt.Errorf("%s: %d is not a valid replica count", ch.target.label(), ch.desired)
	}
	if ch.target.kind != targetControlPlane {
		return nil
	}
	if ch.desired < 1 {
		return fmt.Errorf("control plane: refusing to scale to %d — a cluster with no control plane nodes is gone, not scaled (use 'viti kc delete' if that is the intent)",
			ch.desired)
	}
	if ch.desired%2 == 0 {
		return fmt.Errorf("control plane: %d is even — control plane replicas must be odd (1, 3, 5, ...) to maintain etcd quorum",
			ch.desired)
	}
	return nil
}

// checkScaleFlags validates what can be validated without the cluster: that
// every count parses and that no pool is named twice. It runs before the
// availability zones are contacted, so a typo fails immediately instead of
// after a fleet-wide lookup with its error buried under connection warnings.
//
// It deliberately does not judge the values themselves — "+1" is odd or even
// only once resolved against the cluster's current count.
func checkScaleFlags(cpFlag string, npFlags []string) error {
	if strings.TrimSpace(cpFlag) != "" {
		if _, err := parseCount(cpFlag); err != nil {
			return fmt.Errorf("--controlplane: %w", err)
		}
	}
	seen := make(map[string]struct{}, len(npFlags))
	for _, raw := range npFlags {
		name, _, err := parseNodePoolFlag(raw)
		if err != nil {
			return fmt.Errorf("--nodepool: %w", err)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("--nodepool: node pool %q given more than once — pass one count per pool", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// resolveChanges turns the --controlplane and --nodepool flags into
// validated changes against the cluster as it currently stands. No flags
// means no changes: the caller falls back to the interactive picker.
func resolveChanges(kc *vitiv1alpha1.KubernetesCluster, cpFlag string, npFlags []string) ([]scaleChange, error) {
	targets := clusterTargets(kc)
	var changes []scaleChange

	if strings.TrimSpace(cpFlag) != "" {
		c, err := parseCount(cpFlag)
		if err != nil {
			return nil, fmt.Errorf("--controlplane: %w", err)
		}
		ch, err := newChange(targets[0], c)
		if err != nil {
			return nil, err
		}
		changes = append(changes, ch)
	}

	seen := make(map[string]struct{}, len(npFlags))
	for _, raw := range npFlags {
		name, c, err := parseNodePoolFlag(raw)
		if err != nil {
			return nil, fmt.Errorf("--nodepool: %w", err)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("--nodepool: node pool %q given more than once — pass one count per pool", name)
		}
		seen[name] = struct{}{}
		target, err := findNodePoolTarget(targets, name)
		if err != nil {
			return nil, fmt.Errorf("--nodepool: %w", err)
		}
		ch, err := newChange(target, c)
		if err != nil {
			return nil, err
		}
		changes = append(changes, ch)
	}
	return changes, nil
}

// newChange resolves a count against a target and validates the result.
func newChange(target scaleTarget, c countSpec) (scaleChange, error) {
	desired, err := c.resolve(target.current)
	if err != nil {
		return scaleChange{}, fmt.Errorf("%s: %w", target.label(), err)
	}
	ch := scaleChange{target: target, desired: desired}
	if err := ch.validate(); err != nil {
		return scaleChange{}, err
	}
	return ch, nil
}

// applyChanges writes the resolved counts into the cluster spec. Node pools
// are addressed by index but verified by name first: the index was resolved
// against the spec that was read, and writing it blindly into a list that
// has since been reordered would scale the wrong pool.
func applyChanges(kc *vitiv1alpha1.KubernetesCluster, changes []scaleChange) error {
	pools := kc.Spec.Topology.Workers.NodePools
	for _, ch := range changes {
		if ch.target.kind == targetControlPlane {
			kc.Spec.Topology.ControlPlane.Replicas = ch.desired
			continue
		}
		i := ch.target.index
		if i < 0 || i >= len(pools) {
			return fmt.Errorf("node pool %q is no longer at position %d in the cluster spec — re-run the command",
				ch.target.name, i)
		}
		if pools[i].Name != ch.target.name {
			return fmt.Errorf("node pool at position %d is now %q, not %q — the spec changed underneath; re-run the command",
				i, pools[i].Name, ch.target.name)
		}
		pools[i].Replicas = ch.desired
	}
	return nil
}

// partitionChanges splits the resolved changes into those that write
// something and those already at the requested count.
func partitionChanges(changes []scaleChange) (actionable, noops []scaleChange) {
	for _, ch := range changes {
		if ch.noop() {
			noops = append(noops, ch)
			continue
		}
		actionable = append(actionable, ch)
	}
	return actionable, noops
}

// hasControlPlaneScaleDown reports whether any change removes control plane
// nodes — the case that costs etcd members and so earns the typed-name
// confirmation rather than a bare y/N.
func hasControlPlaneScaleDown(changes []scaleChange) bool {
	for _, ch := range changes {
		if ch.target.kind == targetControlPlane && ch.scalingDown() {
			return true
		}
	}
	return false
}

// writeScalePlan renders what the run will do. Every line names its target,
// node pools by name — on a single-pool cluster no picker was shown, so this
// is the only place the operator learns which pool is being changed.
func writeScalePlan(w io.Writer, az, namespace, name string, actionable, noops []scaleChange) {
	_, _ = fmt.Fprintf(w, "🎯 AZ: %s   namespace: %s   cluster: %s\n", az, namespace, name)
	for _, ch := range actionable {
		_, _ = fmt.Fprintf(w, "   %s\n", ch.describe())
	}
	for _, ch := range noops {
		_, _ = fmt.Fprintf(w, "   %s: already at %d replicas — nothing to do\n",
			ch.target.label(), ch.target.current)
	}
	for _, ch := range actionable {
		if ch.target.autoscaling {
			_, _ = fmt.Fprintf(w,
				"⚠️  %s has autoscaling enabled — the autoscaler may move the count straight back\n",
				ch.target.label())
		}
	}
	if hasControlPlaneScaleDown(actionable) {
		_, _ = fmt.Fprintf(w,
			"⚠️  removing control plane nodes changes etcd membership — quorum is lost if too few members remain\n")
	}
}

var (
	kcScaleAZ             string
	kcScaleNamespace      string
	kcScaleControlPlane   string
	kcScaleNodePools      []string
	kcScaleNodePoolsAlias []string
	kcScaleYes            bool
	kcScaleDryRun         bool
)

var kcScaleCmd = &cobra.Command{
	Use:     "scale [name]",
	Aliases: []string{"resize"},
	Short:   "Change the replica count of a cluster's control plane or a node pool",
	Long: `Scales a KubernetesCluster by writing a new replica count into its spec;
the vitistack operators do the rest.

Counts are absolute ("5") or relative to what the cluster declares today
("+2", "-1"). Relative counts save the arithmetic for "one more worker";
absolute counts keep a scripted invocation idempotent.

Give the target with --controlplane and/or --nodepool <pool>=<count>; both
flags may appear together and --nodepool is repeatable, so several pools
change in a single patch. With neither flag, and a terminal, the cluster and
then the target are chosen interactively. The node pool being changed is
always named in the plan and the result — including on a cluster with only
one pool, where nothing was ambiguous and no picker was shown.

Guardrails: control plane replicas must be odd (etcd quorum) and at least 1,
so an even count is refused here rather than by the apiserver's validation;
removing control plane nodes requires typing the cluster name back; scaling a
pool that has autoscaling enabled warns first. A count equal to the current
one writes nothing at all.

Beyond replica counts, --add-nodepool clones an existing pool into a new one
(unique generated name, machine class and replicas of your choosing) and
--delete-nodepool removes a pool. Both print the exact nodePools entry being
written before asking. Deleting a pool deletes its worker machines, so it
requires typing the pool name back; deleting the last remaining worker pool
is always refused.

Examples:
  viti kc scale                                  # pick cluster, then target
  viti kc scale my-cluster                       # pick the target
  viti kc scale my-cluster --nodepool workers=5
  viti kc scale my-cluster --nodepool workers=+2
  viti kc scale my-cluster --controlplane 3
  viti kc scale my-cluster --cp 3 --np workers=6 --np gpu=-1
  viti kc scale my-cluster --add-nodepool        # pick template, class, count
  viti kc scale my-cluster --add-nodepool --from-pool workers --machineclass xlcpu --replicas 3
  viti kc scale my-cluster --delete-nodepool gpu`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := ""
		if len(args) == 1 {
			name = args[0]
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		// Cheapest checks first: a bad flag must not cost a fleet-wide lookup.
		npFlags := scaleNodePoolFlags()
		if err := checkScaleFlags(kcScaleControlPlane, npFlags); err != nil {
			return err
		}
		if err := checkPoolEditFlags(currentPoolEditFlags(cmd), kcScaleControlPlane, npFlags, picker.Interactive()); err != nil {
			return err
		}

		// Checked before listing anything: with no name and no terminal there
		// is no way to choose, so every zone listing would be wasted work.
		if name == "" && !picker.Interactive() {
			return errors.New("no cluster given — pass one (e.g. 'viti kc scale my-cluster'), " +
				"or run in a terminal to pick one interactively")
		}

		zones, err := kube.ResolveAvailabilityZones(kcScaleAZ)
		if err != nil {
			return err
		}
		clients, err := kube.ConnectAll(ctx, zones, true, warn)
		if err != nil {
			return err
		}

		// One terminal session for the whole interactive flow: cluster pick,
		// target pick, and every popup share it, so nothing flashes between
		// steps. Opened lazily so a fully flag-driven run never touches the
		// terminal, and closed before anything prints or prompts on stdio.
		var sess *picker.Session
		ensureSession := func() (*picker.Session, error) {
			if sess != nil {
				return sess, nil
			}
			s, err := picker.NewSession()
			if err != nil {
				return nil, err
			}
			sess = s
			return sess, nil
		}
		closeSession := func() {
			if sess != nil {
				sess.Close()
			}
		}
		defer closeSession()

		var hit *kcHit
		if name != "" {
			// A mutation must not act on a name that a zone we could not
			// reach might also hold.
			if err := requireWholeFleet(clients, zones, "cluster", name); err != nil {
				return err
			}
			hit, err = findClusterAcrossAZs(ctx, clients, name, kcScaleNamespace)
		} else {
			var s *picker.Session
			s, err = ensureSession()
			if err != nil {
				return err
			}
			hit, err = pickClusterToScale(ctx, s, clients)
		}
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()

		// Pool edits run their own plan/confirm/patch sequence — the session
		// must be closed first: the plan and the prompts use plain stdio.
		if poolEditRequested() {
			edit, err := resolvePoolEditFromFlags(ctx, ensureSession, hit)
			closeSession()
			if err != nil {
				return err
			}
			return runPoolEdit(ctx, cmd, hit, *edit)
		}

		changes, err := resolveChanges(hit.cluster, kcScaleControlPlane, npFlags)
		if err != nil {
			return err
		}
		if len(changes) == 0 {
			var edit *poolEdit
			changes, edit, err = promptForChange(ctx, ensureSession, hit)
			if err != nil {
				return err
			}
			if edit != nil {
				closeSession()
				return runPoolEdit(ctx, cmd, hit, *edit)
			}
		}

		// The plan, the confirmation prompt, and the patch output all use the
		// plain terminal.
		closeSession()

		actionable, noops := partitionChanges(changes)
		writeScalePlan(out, hit.client.AZ.Name, hit.cluster.Namespace, hit.cluster.Name, actionable, noops)

		if len(actionable) == 0 {
			_, _ = fmt.Fprintln(out, "✅ nothing to change")
			return nil
		}
		if kcScaleDryRun {
			_, _ = fmt.Fprintln(out, "🔍 dry run — nothing was changed")
			return nil
		}
		if !kcScaleYes {
			if err := confirmScale(cmd, hit.cluster.Name, actionable); err != nil {
				return err
			}
		}

		key := ctrlclient.ObjectKeyFromObject(hit.cluster)
		if _, err := patchScale(ctx, hit.client.Ctrl, key, actionable); err != nil {
			if apierrors.IsConflict(err) {
				return fmt.Errorf("kubernetescluster %s/%s is being rewritten faster than this command can patch it — re-run it: %w",
					hit.cluster.Namespace, hit.cluster.Name, err)
			}
			return fmt.Errorf("patching kubernetescluster %s/%s: %w", hit.cluster.Namespace, hit.cluster.Name, err)
		}

		_, _ = fmt.Fprintf(out, "✅ patched kubernetescluster/%s/%s\n", hit.cluster.Namespace, hit.cluster.Name)
		for _, ch := range actionable {
			_, _ = fmt.Fprintf(out, "   %s\n", ch.describe())
		}
		_, _ = fmt.Fprintf(out, "   follow with: viti kc get %s\n", hit.cluster.Name)
		_, _ = fmt.Fprintf(out, "                viti kc health %s\n", hit.cluster.Name)
		return nil
	},
}

// pickClusterToScale shows every cluster in scope and returns the chosen one.
func pickClusterToScale(ctx context.Context, sess *picker.Session, clients []*kube.Client) (*kcHit, error) {
	hits := collectClusters(ctx, clients, kcScaleNamespace)
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
			// Matched on every column, so an environment or a clusterId
			// narrows the list as readily as a name does.
			Label:   strings.Join(columns, " "),
			Columns: columns,
			Value:   &hits[i],
		})
	}
	chosen, err := sess.Select(" Select a cluster to scale ",
		[]string{"AZ", "NAMESPACE", "NAME", "CLUSTER ID", "PROVIDER", "ENV", "PHASE", "CP", "POOLS"}, items)
	if err != nil {
		if errors.Is(err, picker.ErrCancelled) {
			return nil, errors.New("aborted")
		}
		return nil, err
	}
	got, ok := chosen.Value.(*kcHit)
	if !ok {
		return nil, fmt.Errorf("picker returned an unexpected item %T", chosen.Value)
	}
	return got, nil
}

// scaleAction is what the interactive target picker resolves to: a replica
// change on the chosen target, or one of the pool edits.
type scaleAction int

const (
	actionScaleReplicas scaleAction = iota
	actionAddPool
	actionDeletePool
)

// promptForChange resolves the target and count interactively, for a run that
// named neither on the command line. Choosing one of the pool-edit rows
// returns a poolEdit instead of changes.
//
// It loops: backing out of a popup (Esc) returns to the target picker rather
// than aborting the command, so a wrong turn costs one keystroke, not a
// re-run. Only cancelling the target picker itself aborts.
func promptForChange(ctx context.Context, ensure func() (*picker.Session, error), hit *kcHit) ([]scaleChange, *poolEdit, error) {
	if !picker.Interactive() {
		return nil, nil, errors.New("no target given — pass --controlplane <count> and/or --nodepool <pool>=<count>, " +
			"or run in a terminal to pick one interactively")
	}
	sess, err := ensure()
	if err != nil {
		return nil, nil, err
	}
	kc := hit.cluster
	for {
		target, action, err := pickScaleTarget(sess, kc)
		if err != nil {
			return nil, nil, err
		}
		switch action {
		case actionAddPool:
			edit, err := resolveAddPool(ctx, ensure, hit)
			if errors.Is(err, picker.ErrCancelled) {
				continue // backed out of the first step: the target list again
			}
			return nil, edit, err
		case actionDeletePool:
			pools := kc.Spec.Topology.Workers.NodePools
			if len(pools) == 0 {
				return nil, nil, errors.New("this cluster declares no node pools — there is nothing to delete")
			}
			if len(pools) == 1 {
				return nil, nil, errLastPool(pools[0].Name)
			}
			i, err := pickPool(sess, kc, " Select the pool to delete from "+kc.Name+" ")
			if errors.Is(err, picker.ErrCancelled) {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			return nil, &poolEdit{kind: poolEditDelete, poolName: pools[i].Name, poolIndex: i}, nil
		}

		prompt := fmt.Sprintf("%s currently has %d replicas", target.label(), target.current)
		answer, err := sess.Input(" New replica count ", prompt, "e.g. 5, +2, -1", validateCountFor(target))
		if errors.Is(err, picker.ErrCancelled) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		count, err := parseCount(answer)
		if err != nil {
			return nil, nil, err
		}
		ch, err := newChange(target, count)
		if err != nil {
			return nil, nil, err
		}
		return []scaleChange{ch}, nil, nil
	}
}

// validateCountFor judges an answer against a target while it is being typed
// into the popup, so "4 is even" or "cannot scale below zero" appears inline
// instead of after the terminal is torn down.
func validateCountFor(target scaleTarget) func(string) error {
	return func(v string) error {
		c, err := parseCount(v)
		if err != nil {
			return err
		}
		_, err = newChange(target, c)
		return err
	}
}

// pickScaleTarget shows the control plane and every node pool, so choosing
// between workers and the control plane is the same act as choosing between
// two pools. Below the targets sit the two pool-edit actions, so adding or
// deleting a pool starts from the same picker as scaling one.
func pickScaleTarget(sess *picker.Session, kc *vitiv1alpha1.KubernetesCluster) (scaleTarget, scaleAction, error) {
	targets := clusterTargets(kc)
	items := make([]picker.Item, 0, len(targets)+2)
	for _, t := range targets {
		autoscaling := "-"
		if t.autoscaling {
			autoscaling = "yes"
		}
		columns := []string{
			t.label(), strconv.Itoa(t.current), valueOrDash(t.machineClass), autoscaling,
		}
		items = append(items, picker.Item{
			Label:   strings.Join(columns, " "),
			Columns: columns,
			Value:   t,
		})
	}
	for _, a := range []struct {
		label  string
		action scaleAction
	}{
		{"➕ add a node pool", actionAddPool},
		{"🗑  delete a node pool", actionDeletePool},
	} {
		items = append(items, picker.Item{
			Label:   a.label,
			Columns: []string{a.label, "", "", ""},
			Value:   a.action,
		})
	}
	chosen, err := sess.Select(" Select what to scale in "+kc.Name+" ",
		[]string{"TARGET", "REPLICAS", "MACHINE CLASS", "AUTOSCALING"}, items)
	if err != nil {
		if errors.Is(err, picker.ErrCancelled) {
			return scaleTarget{}, actionScaleReplicas, errors.New("aborted")
		}
		return scaleTarget{}, actionScaleReplicas, err
	}
	switch v := chosen.Value.(type) {
	case scaleTarget:
		return v, actionScaleReplicas, nil
	case scaleAction:
		return scaleTarget{}, v, nil
	default:
		return scaleTarget{}, actionScaleReplicas, fmt.Errorf("picker returned an unexpected item %T", chosen.Value)
	}
}

// verifyTargetsUnchanged checks that every target still holds the replica
// count the operator was shown before confirming.
//
// This — not the object's resourceVersion — is the property worth guarding.
// The vitistack operators rewrite status every few seconds, so a
// resourceVersion guard fires constantly on changes that are none of our
// business, while a target whose count actually moved is the one case where
// applying an approved "3 -> 4" would land somewhere the operator never saw.
// A concurrent change to a pool this command is not touching is preserved,
// because the patch is built from a freshly read object.
func verifyTargetsUnchanged(fresh *vitiv1alpha1.KubernetesCluster, changes []scaleChange) error {
	for _, ch := range changes {
		if ch.target.kind == targetControlPlane {
			if got := fresh.Spec.Topology.ControlPlane.Replicas; got != ch.target.current {
				return fmt.Errorf(
					"control plane is now at %d replicas, not the %d this command was about to scale from — re-run it",
					got, ch.target.current)
			}
			continue
		}
		found := false
		for i := range fresh.Spec.Topology.Workers.NodePools {
			p := &fresh.Spec.Topology.Workers.NodePools[i]
			if p.Name != ch.target.name {
				continue
			}
			found = true
			if p.Replicas != ch.target.current {
				return fmt.Errorf(
					"node pool %q is now at %d replicas, not the %d this command was about to scale from — re-run it",
					ch.target.name, p.Replicas, ch.target.current)
			}
			ch.target.index = i
		}
		if !found {
			return fmt.Errorf("node pool %q is no longer on this cluster — re-run the command", ch.target.name)
		}
	}
	return nil
}

// patchScale writes the approved counts, re-reading the cluster on each
// attempt.
//
// The re-read is the point: an interactive run spends seconds at the prompt,
// and the object it was picked from is stale by the time the answer arrives.
// Conflicts are retried because they mean status churn raced the write;
// a target that genuinely moved aborts instead, and is not retried.
func patchScale(ctx context.Context, c ctrlclient.Client, key ctrlclient.ObjectKey, changes []scaleChange) (*vitiv1alpha1.KubernetesCluster, error) {
	if len(changes) == 0 {
		return nil, errors.New("nothing to patch")
	}
	var out *vitiv1alpha1.KubernetesCluster
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &vitiv1alpha1.KubernetesCluster{}
		if err := c.Get(ctx, key, fresh); err != nil {
			return err
		}
		if err := verifyTargetsUnchanged(fresh, changes); err != nil {
			// Not a conflict: retrying would re-read the same moved value.
			return err
		}
		base := fresh.DeepCopy()
		if err := applyChanges(fresh, changes); err != nil {
			return err
		}
		// The lock is cheap here — the read is milliseconds old, so it guards
		// the write window rather than the operator's whole prompt.
		patch := ctrlclient.MergeFromWithOptions(base, ctrlclient.MergeFromWithOptimisticLock{})
		if err := c.Patch(ctx, fresh, patch); err != nil {
			return err
		}
		out = fresh
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scaleNodePoolFlags returns every --nodepool value given, from the long
// flag and its alias alike. The two cannot share one slice: pflag resets a
// string-array flag's target on that flag's first Set, so binding both to
// the same variable makes whichever appears second discard the other's
// values.
func scaleNodePoolFlags() []string {
	out := make([]string, 0, len(kcScaleNodePools)+len(kcScaleNodePoolsAlias))
	out = append(out, kcScaleNodePools...)
	out = append(out, kcScaleNodePoolsAlias...)
	return out
}

// confirmScale asks for consent proportional to the change. Removing control
// plane nodes shrinks etcd membership, so it takes the cluster name typed
// back rather than a keystroke; everything else is a y/N.
func confirmScale(cmd *cobra.Command, name string, actionable []scaleChange) error {
	if hasControlPlaneScaleDown(actionable) {
		return confirmTypedName(cmd, "cluster", "removing control plane nodes", name)
	}
	ok, err := confirm(cmd, "Apply?")
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("aborted")
	}
	return nil
}

func init() {
	kcScaleCmd.Flags().StringVarP(&kcScaleAZ, "availabilityzone", "z", "",
		"restrict the search to a single availability zone")
	kcScaleCmd.Flags().StringVarP(&kcScaleNamespace, "namespace", "n", "",
		"namespace of the KubernetesCluster")
	kcScaleCmd.Flags().StringVar(&kcScaleControlPlane, "controlplane", "",
		"control plane replica count: absolute (3) or relative (+2, -1); must stay odd")
	kcScaleCmd.Flags().StringVar(&kcScaleControlPlane, "cp", "",
		"alias for --controlplane")
	kcScaleCmd.Flags().StringArrayVar(&kcScaleNodePools, "nodepool", nil,
		"node pool replica count as <pool>=<count>, absolute (workers=5) or relative (workers=+2); repeatable")
	kcScaleCmd.Flags().StringArrayVar(&kcScaleNodePoolsAlias, "np", nil,
		"alias for --nodepool")
	kcScaleCmd.Flags().BoolVar(&kcScaleYes, "yes", false, "skip the confirmation prompt")
	kcScaleCmd.Flags().BoolVar(&kcScaleDryRun, "dry-run", false,
		"resolve and print the plan without changing anything")

	kubernetesClusterCmd.AddCommand(kcScaleCmd)
}
