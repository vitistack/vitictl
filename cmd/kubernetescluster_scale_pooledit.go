package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/pkg/plugin/picker"
)

// poolEditKind distinguishes the two structural changes to the node pool
// list, as opposed to the replica changes scaleChange models.
type poolEditKind int

const (
	poolEditAdd poolEditKind = iota
	poolEditDelete
)

// poolEdit is a resolved structural change to spec.topology.workers.nodePools.
//
// It is deliberately not a scaleChange: everything downstream of scaleChange
// (describe, validate, applyChanges, verifyTargetsUnchanged, patchScale)
// assumes "target plus desired count", while a pool edit changes the shape of
// the list itself. It gets its own verify/apply/patch path instead, built on
// the same retry-and-lock mechanics.
type poolEdit struct {
	kind poolEditKind
	// newPool is the fully resolved entry to append (add only).
	newPool vitiv1alpha1.KubernetesClusterNodePool
	// template names the pool the new one was cloned from (add only), for
	// the plan output.
	template string
	// poolName and poolIndex identify the pool to remove (delete only).
	poolName  string
	poolIndex int
}

var (
	kcScaleAddNodePool    bool
	kcScaleDeleteNodePool string
	kcScaleFromPool       string
	kcScaleMachineClass   string
	kcScaleReplicas       string
	kcScalePoolName       string
)

// poolEditFlags carries the pool-edit flag values into the pure validation
// function, so the flag matrix is testable without a cobra command.
type poolEditFlags struct {
	add        bool
	deletePool string
	// deleteChanged distinguishes an absent --delete-nodepool from an empty
	// one: "" with the flag given is a mistake worth naming, not a no-op.
	deleteChanged bool
	fromPool      string
	machineClass  string
	replicas      string
	name          string
}

func currentPoolEditFlags(cmd *cobra.Command) poolEditFlags {
	return poolEditFlags{
		add:           kcScaleAddNodePool,
		deletePool:    strings.TrimSpace(kcScaleDeleteNodePool),
		deleteChanged: cmd.Flags().Changed("delete-nodepool"),
		fromPool:      strings.TrimSpace(kcScaleFromPool),
		machineClass:  strings.TrimSpace(kcScaleMachineClass),
		replicas:      strings.TrimSpace(kcScaleReplicas),
		name:          strings.TrimSpace(kcScalePoolName),
	}
}

// poolEditRequested reports whether this run is a pool edit rather than a
// replica change.
func poolEditRequested() bool {
	return kcScaleAddNodePool || strings.TrimSpace(kcScaleDeleteNodePool) != ""
}

// checkPoolEditFlags validates the pool-edit flag matrix before any zone is
// contacted, mirroring checkScaleFlags: a contradiction between flags must
// not cost a fleet-wide lookup.
//
// One action per run: an add or a delete excludes the other and excludes the
// replica flags. The values themselves are judged only as far as syntax
// allows — name uniqueness needs the cluster.
func checkPoolEditFlags(f poolEditFlags, cpFlag string, npFlags []string, interactive bool) error {
	if f.deleteChanged && f.deletePool == "" {
		return errors.New("--delete-nodepool: empty pool name — name the pool to delete")
	}
	if f.add && f.deletePool != "" {
		return errors.New("--add-nodepool and --delete-nodepool are mutually exclusive — one action per run")
	}
	if !f.add {
		var extras []string
		for _, given := range []struct{ flag, value string }{
			{"--from-pool", f.fromPool},
			{"--machineclass", f.machineClass},
			{"--replicas", f.replicas},
			{"--name", f.name},
		} {
			if given.value != "" {
				extras = append(extras, given.flag)
			}
		}
		if len(extras) > 0 {
			return fmt.Errorf("%s only applies with --add-nodepool", strings.Join(extras, ", "))
		}
	}
	if !f.add && f.deletePool == "" {
		return nil
	}
	if strings.TrimSpace(cpFlag) != "" || len(npFlags) > 0 {
		return errors.New("--add-nodepool/--delete-nodepool cannot be combined with --controlplane or --nodepool — one action per run")
	}
	if f.replicas != "" {
		if _, err := parseNewPoolReplicas(f.replicas); err != nil {
			return fmt.Errorf("--replicas: %w", err)
		}
	}
	if f.name != "" {
		if err := validatePoolName(f.name, nil); err != nil {
			return fmt.Errorf("--name: %w", err)
		}
	}
	if f.add && !interactive {
		if f.machineClass == "" {
			return errors.New("--add-nodepool without a terminal needs --machineclass — there is no picker to choose one from")
		}
		if f.replicas == "" {
			return errors.New("--add-nodepool without a terminal needs --replicas — there is no prompt to ask with")
		}
	}
	return nil
}

// parseNewPoolReplicas reads the replica count for a pool that does not
// exist yet. Deltas are meaningless without a current count, and an empty
// new pool is almost certainly a mistake, so both are refused.
func parseNewPoolReplicas(s string) (int, error) {
	c, err := parseCount(s)
	if err != nil {
		return 0, err
	}
	if c.delta {
		return 0, errors.New("a new pool has no current count — give an absolute number (e.g. 3)")
	}
	if c.value < 1 {
		return 0, fmt.Errorf("a new pool needs at least 1 replica, not %d", c.value)
	}
	return c.value, nil
}

// validatePoolName checks that a name would be accepted by the apiserver and
// is not already taken on the cluster. taken may be nil for a pure syntax
// check.
func validatePoolName(name string, taken map[string]struct{}) error {
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return fmt.Errorf("%q is not a valid node pool name: %s", name, strings.Join(errs, "; "))
	}
	if _, dup := taken[name]; dup {
		return fmt.Errorf("this cluster already has a node pool named %q", name)
	}
	return nil
}

// newPoolName generates a unique workers-<8 hex> name. Collisions are next
// to impossible, but a collision that did slip through would silently patch
// an existing pool in a later iteration, so they are checked anyway.
func newPoolName(taken map[string]struct{}) (string, error) {
	for attempt := 0; attempt < 10; attempt++ {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("generating a pool name: %w", err)
		}
		name := "workers-" + hex.EncodeToString(b)
		if _, dup := taken[name]; !dup {
			return name, nil
		}
	}
	return "", errors.New("could not generate an unused pool name — give one explicitly with --name")
}

// poolNames returns the names already in use on the cluster.
func poolNames(pools []vitiv1alpha1.KubernetesClusterNodePool) map[string]struct{} {
	taken := make(map[string]struct{}, len(pools))
	for i := range pools {
		taken[pools[i].Name] = struct{}{}
	}
	return taken
}

// cloneNodePool copies a template pool and overrides the three fields a new
// pool gets of its own. The deep copy matters: the template's Taint, Storage
// and ScalingRules slices must not be shared with the new entry.
func cloneNodePool(tpl vitiv1alpha1.KubernetesClusterNodePool, name, machineClass string, replicas int) vitiv1alpha1.KubernetesClusterNodePool {
	out := *tpl.DeepCopy()
	out.Name = name
	out.MachineClass = machineClass
	out.Replicas = replicas
	return out
}

// resolveTemplatePool decides which pool a new one is cloned from. An
// explicit --from-pool wins; a single pool is unambiguous; several pools
// need the picker (needPicker) or, without a terminal, an explicit flag.
func resolveTemplatePool(pools []vitiv1alpha1.KubernetesClusterNodePool, fromPool string, interactive bool) (tpl *vitiv1alpha1.KubernetesClusterNodePool, needPicker bool, err error) {
	if len(pools) == 0 {
		return nil, false, errors.New("this cluster declares no worker pools — a new pool is cloned from an existing one, so there is nothing to clone from")
	}
	names := make([]string, 0, len(pools))
	for i := range pools {
		names = append(names, strconv.Quote(pools[i].Name))
	}
	if fromPool != "" {
		for i := range pools {
			if pools[i].Name == fromPool {
				return &pools[i], false, nil
			}
		}
		return nil, false, fmt.Errorf("--from-pool: no node pool named %q on this cluster — it has %s",
			fromPool, strings.Join(names, ", "))
	}
	if len(pools) == 1 {
		return &pools[0], false, nil
	}
	if interactive {
		return nil, true, nil
	}
	return nil, false, fmt.Errorf("this cluster has several node pools — say which to clone with --from-pool (it has %s)",
		strings.Join(names, ", "))
}

// errLastPool is the refusal both delete paths give: a cluster whose last
// worker pool is removed has no workers at all, which is decommissioning,
// not scaling.
func errLastPool(name string) error {
	return fmt.Errorf("refusing to delete node pool %q — it is the cluster's last worker pool", name)
}

// checkPoolDelete resolves a pool name for deletion and applies the
// guardrails: the pool must exist, and it must not be the last one.
func checkPoolDelete(pools []vitiv1alpha1.KubernetesClusterNodePool, name string) (int, error) {
	if len(pools) == 0 {
		return 0, errors.New("this cluster declares no node pools — there is nothing to delete")
	}
	idx := -1
	names := make([]string, 0, len(pools))
	for i := range pools {
		if pools[i].Name == name {
			idx = i
			continue
		}
		names = append(names, strconv.Quote(pools[i].Name))
	}
	if idx < 0 {
		return 0, fmt.Errorf("no node pool named %q on this cluster — it has %s", name, strings.Join(names, ", "))
	}
	if len(pools) == 1 {
		return 0, errLastPool(name)
	}
	return idx, nil
}

// resolvePoolEditFromFlags turns the pool-edit flags into a resolved edit
// against the chosen cluster, filling anything not given on the command line
// interactively. This is the flag-path entry, so backing out of the first
// interactive step means aborting the command.
func resolvePoolEditFromFlags(ctx context.Context, ensure func() (*picker.Session, error), hit *kcHit) (*poolEdit, error) {
	pools := hit.cluster.Spec.Topology.Workers.NodePools
	if name := strings.TrimSpace(kcScaleDeleteNodePool); name != "" {
		i, err := checkPoolDelete(pools, name)
		if err != nil {
			return nil, err
		}
		return &poolEdit{kind: poolEditDelete, poolName: name, poolIndex: i}, nil
	}
	edit, err := resolveAddPool(ctx, ensure, hit)
	if errors.Is(err, picker.ErrCancelled) {
		return nil, errors.New("aborted")
	}
	return edit, err
}

// addPoolStep is one interactive question of the add flow, in the order they
// are asked. The order matters twice: the replicas prompt names the template,
// and Esc walks the same order backwards.
type addPoolStep int

const (
	stepTemplate addPoolStep = iota
	// stepClassAndReplicas asks both in one popup — the usual interactive
	// case. The solo steps exist for runs where a flag answered one of the
	// two already.
	stepClassAndReplicas
	stepMachineClass
	stepReplicas
)

// resolveAddPool assembles the new pool: template, machine class, replicas
// and name, each from its flag when given and via a popup otherwise. The
// popups form a small wizard: Esc steps back to the previous question, and
// Esc on the first one surfaces as picker.ErrCancelled for the caller to
// translate — "back to the target list" interactively, "aborted" on the
// flag path.
func resolveAddPool(ctx context.Context, ensure func() (*picker.Session, error), hit *kcHit) (*poolEdit, error) {
	pools := hit.cluster.Spec.Topology.Workers.NodePools

	tpl, needPicker, err := resolveTemplatePool(pools, strings.TrimSpace(kcScaleFromPool), picker.Interactive())
	if err != nil {
		return nil, err
	}

	machineClass := strings.TrimSpace(kcScaleMachineClass)
	replicas := 0
	replicasGiven := strings.TrimSpace(kcScaleReplicas) != ""
	if replicasGiven {
		replicas, err = parseNewPoolReplicas(strings.TrimSpace(kcScaleReplicas))
		if err != nil {
			return nil, fmt.Errorf("--replicas: %w", err)
		}
	}

	var steps []addPoolStep
	if needPicker {
		steps = append(steps, stepTemplate)
	}
	switch {
	case machineClass == "" && !replicasGiven:
		steps = append(steps, stepClassAndReplicas)
	case machineClass == "":
		steps = append(steps, stepMachineClass)
	case !replicasGiven:
		steps = append(steps, stepReplicas)
	}

	if len(steps) > 0 {
		sess, err := ensure()
		if err != nil {
			return nil, err
		}
		sess.SetContext(fmt.Sprintf("viti kc scale — %s (%s/%s)",
			hit.cluster.Name, hit.client.AZ.Name, hit.cluster.Namespace))
		for i := 0; i < len(steps); {
			var stepErr error
			switch steps[i] {
			case stepTemplate:
				var idx int
				idx, stepErr = pickPool(sess, hit.cluster, " Select the pool to clone in "+hit.cluster.Name+" ")
				if stepErr == nil {
					tpl = &pools[idx]
				}
			case stepClassAndReplicas:
				machineClass, replicas, stepErr = pickMachineClassAndReplicas(ctx, sess, hit.client, tpl)
			case stepMachineClass:
				machineClass, stepErr = pickMachineClass(ctx, sess, hit.client)
			case stepReplicas:
				replicas, stepErr = inputReplicas(sess, tpl)
			}
			switch {
			case stepErr == nil:
				i++
			case errors.Is(stepErr, picker.ErrCancelled):
				if i == 0 {
					return nil, stepErr
				}
				i-- // Esc: back to the previous question
			default:
				return nil, stepErr
			}
		}
	}

	taken := poolNames(pools)
	name := strings.TrimSpace(kcScalePoolName)
	if name != "" {
		if err := validatePoolName(name, taken); err != nil {
			return nil, fmt.Errorf("--name: %w", err)
		}
	} else {
		name, err = newPoolName(taken)
		if err != nil {
			return nil, err
		}
	}

	template := tpl.Name
	if template == "" {
		template = "<unnamed pool>"
	}
	return &poolEdit{
		kind:     poolEditAdd,
		newPool:  cloneNodePool(*tpl, name, machineClass, replicas),
		template: template,
	}, nil
}

// pickPool shows the cluster's node pools in a popup over the current screen
// and returns the chosen index. Cancelling returns picker.ErrCancelled raw:
// the callers' step loops read it as "back", not as an abort.
func pickPool(sess *picker.Session, kc *vitiv1alpha1.KubernetesCluster, title string) (int, error) {
	pools := kc.Spec.Topology.Workers.NodePools
	items := make([]picker.Item, 0, len(pools))
	for i := range pools {
		name := pools[i].Name
		if name == "" {
			name = fmt.Sprintf("<unnamed pool #%d>", i)
		}
		autoscaling := "-"
		if pools[i].Autoscaling.Enabled {
			autoscaling = "yes"
		}
		columns := []string{name, strconv.Itoa(pools[i].Replicas), valueOrDash(pools[i].MachineClass), autoscaling}
		items = append(items, picker.Item{
			Label:   strings.Join(columns, " "),
			Columns: columns,
			Value:   i,
		})
	}
	chosen, err := sess.SelectPopup(title, []string{"NAME", "REPLICAS", "MACHINE CLASS", "AUTOSCALING"}, items)
	if err != nil {
		return 0, err
	}
	i, ok := chosen.Value.(int)
	if !ok {
		return 0, fmt.Errorf("picker returned an unexpected item %T", chosen.Value)
	}
	return i, nil
}

// machineClassHeader are the columns every machine-class list shows.
var machineClassHeader = []string{"NAME", "DISPLAY NAME", "CATEGORY", "CPU", "MEMORY", "DEFAULT"}

// enabledMachineClasses fetches the zone's usable classes, sorted by name so
// the list reads like `viti mc` output. The MachineClass CRD is not in
// kube.RequiredCRDs, so instead of an error the second return is a
// human-readable reason the picker cannot be offered — the caller degrades to
// a typed answer. Nothing may write to stderr here: the terminal belongs to
// the session, and a warn() would be painted over the screen.
func enabledMachineClasses(ctx context.Context, c *kube.Client) ([]vitiv1alpha1.MachineClass, string) {
	list := &vitiv1alpha1.MachineClassList{}
	if err := c.Ctrl.List(ctx, list); err != nil {
		return nil, fmt.Sprintf("listing machine classes in %s failed — type one instead", c.AZ.Name)
	}
	classes := make([]vitiv1alpha1.MachineClass, 0, len(list.Items))
	for _, mc := range list.Items {
		if mc.Spec.Enabled {
			classes = append(classes, mc)
		}
	}
	if len(classes) == 0 {
		return nil, fmt.Sprintf("no enabled machine classes found in %s — type one instead", c.AZ.Name)
	}
	sortMachineClassesByName(classes)
	return classes, ""
}

// sortMachineClassesByName orders the picker rows by name alone. The classes
// are named to be told apart, so name order is the one the operator can
// predict — sorting by category and size first made related names land far
// apart.
func sortMachineClassesByName(classes []vitiv1alpha1.MachineClass) {
	sort.Slice(classes, func(i, j int) bool {
		return classes[i].Name < classes[j].Name
	})
}

// machineClassItems renders classes as picker rows carrying the class name.
func machineClassItems(classes []vitiv1alpha1.MachineClass) []picker.Item {
	items := make([]picker.Item, 0, len(classes))
	for i := range classes {
		mc := &classes[i]
		def := "-"
		if mc.Spec.Default {
			def = "yes"
		}
		columns := []string{
			mc.Name, valueOrDash(mc.Spec.DisplayName), valueOrDash(mc.Spec.Category),
			strconv.FormatUint(uint64(mc.Spec.CPU.Cores), 10), mc.Spec.Memory.Quantity.String(), def,
		}
		items = append(items, picker.Item{
			Label:   strings.Join(columns, " "),
			Columns: columns,
			Value:   mc.Name,
		})
	}
	return items
}

// pickMachineClass asks only for the class, for a run where --replicas was
// already given on the command line.
func pickMachineClass(ctx context.Context, sess *picker.Session, c *kube.Client) (string, error) {
	classes, reason := enabledMachineClasses(ctx, c)
	if reason != "" {
		return inputMachineClass(sess, reason)
	}
	chosen, err := sess.SelectPopup(" Select a machine class for the new pool ",
		machineClassHeader, machineClassItems(classes))
	if err != nil {
		return "", err
	}
	name, ok := chosen.Value.(string)
	if !ok {
		return "", fmt.Errorf("picker returned an unexpected item %T", chosen.Value)
	}
	return name, nil
}

// pickMachineClassAndReplicas asks both remaining questions in one popup: the
// class from the list, the count in the field beneath it. When the classes
// cannot be listed it degrades to two input popups with the same
// back-navigation between them.
func pickMachineClassAndReplicas(ctx context.Context, sess *picker.Session, c *kube.Client,
	tpl *vitiv1alpha1.KubernetesClusterNodePool) (string, int, error) {
	classes, reason := enabledMachineClasses(ctx, c)
	if reason != "" {
		for {
			machineClass, err := inputMachineClass(sess, reason)
			if err != nil {
				return "", 0, err
			}
			replicas, err := inputReplicas(sess, tpl)
			if errors.Is(err, picker.ErrCancelled) {
				continue // Esc: back to the machine-class question
			}
			if err != nil {
				return "", 0, err
			}
			return machineClass, replicas, nil
		}
	}

	tplName := tpl.Name
	if tplName == "" {
		tplName = "<unnamed pool>"
	}
	title := fmt.Sprintf(" New node pool — clone of %q ", tplName)
	placeholder := fmt.Sprintf("e.g. 3 (template has %d)", tpl.Replicas)
	chosen, answer, err := sess.SelectPopupWithInput(title,
		machineClassHeader, machineClassItems(classes),
		"machine class", "replicas", placeholder, validateNewPoolReplicas)
	if err != nil {
		return "", 0, err
	}
	name, ok := chosen.Value.(string)
	if !ok {
		return "", 0, fmt.Errorf("picker returned an unexpected item %T", chosen.Value)
	}
	replicas, err := parseNewPoolReplicas(answer)
	if err != nil {
		return "", 0, err
	}
	return name, replicas, nil
}

// inputMachineClass is the fallback when the machine classes cannot be
// listed: the operator types the name they already know, into a popup that
// states why the list is not on offer.
func inputMachineClass(sess *picker.Session, prompt string) (string, error) {
	return sess.Input(" Machine class for the new pool ", prompt, "", validateMachineClass)
}

// validateMachineClass refuses an empty answer; the value itself can only be
// judged by the apiserver.
func validateMachineClass(v string) error {
	if v == "" {
		return errors.New("empty machine class — the new pool needs one")
	}
	return nil
}

// inputReplicas asks for the new pool's replica count in a popup, naming the
// template and its count so the answer is given with the starting point in
// view.
func inputReplicas(sess *picker.Session, tpl *vitiv1alpha1.KubernetesClusterNodePool) (int, error) {
	tplName := tpl.Name
	if tplName == "" {
		tplName = "<unnamed pool>"
	}
	prompt := fmt.Sprintf("template pool %q has %d replicas", tplName, tpl.Replicas)
	answer, err := sess.Input(" Replicas for the new pool ", prompt, "e.g. 3", validateNewPoolReplicas)
	if err != nil {
		return 0, err
	}
	return parseNewPoolReplicas(answer)
}

// validateNewPoolReplicas is parseNewPoolReplicas as a popup validator.
func validateNewPoolReplicas(v string) error {
	_, err := parseNewPoolReplicas(v)
	return err
}

// writePoolEditPlan renders the edit exactly as it will land in
// spec.topology.workers.nodePools, before anything is written — the same
// contract as writeScalePlan: what the operator confirms is what is patched.
func writePoolEditPlan(w io.Writer, az, namespace, name string, edit poolEdit, pools []vitiv1alpha1.KubernetesClusterNodePool) error {
	_, _ = fmt.Fprintf(w, "🎯 AZ: %s   namespace: %s   cluster: %s\n", az, namespace, name)
	var after []vitiv1alpha1.KubernetesClusterNodePool
	switch edit.kind {
	case poolEditAdd:
		_, _ = fmt.Fprintf(w, "➕ node pool %q: clone of %q — machine class %q, %d replicas\n",
			edit.newPool.Name, edit.template, edit.newPool.MachineClass, edit.newPool.Replicas)
		_, _ = fmt.Fprintln(w, "   will append to spec.topology.workers.nodePools:")
		if err := writePoolYAML(w, edit.newPool); err != nil {
			return err
		}
		after = append(append([]vitiv1alpha1.KubernetesClusterNodePool{}, pools...), edit.newPool)
		if edit.newPool.Autoscaling.Enabled {
			_, _ = fmt.Fprintf(w, "⚠️  the template has autoscaling enabled — the new pool inherits it\n")
		}
	case poolEditDelete:
		if edit.poolIndex < 0 || edit.poolIndex >= len(pools) || pools[edit.poolIndex].Name != edit.poolName {
			return fmt.Errorf("node pool %q is no longer at position %d in the cluster spec — re-run the command",
				edit.poolName, edit.poolIndex)
		}
		removed := pools[edit.poolIndex]
		_, _ = fmt.Fprintf(w, "🗑  node pool %q: %d replicas, machine class %q\n",
			edit.poolName, removed.Replicas, removed.MachineClass)
		_, _ = fmt.Fprintln(w, "   will remove from spec.topology.workers.nodePools:")
		if err := writePoolYAML(w, removed); err != nil {
			return err
		}
		after = append(after, pools[:edit.poolIndex]...)
		after = append(after, pools[edit.poolIndex+1:]...)
		_, _ = fmt.Fprintf(w, "⚠️  removing a node pool deletes its worker machines\n")
	default:
		return fmt.Errorf("unknown pool edit kind %d", edit.kind)
	}
	writePoolTable(w, after)
	return nil
}

// writePoolYAML renders one pool as a nodePools list entry — the exact block
// an operator pastes into kubectl edit, hence the marshal of a one-element
// slice ("- machineClass: ..." rather than a bare mapping).
func writePoolYAML(w io.Writer, p vitiv1alpha1.KubernetesClusterNodePool) error {
	raw, err := yaml.Marshal([]vitiv1alpha1.KubernetesClusterNodePool{p})
	if err != nil {
		return fmt.Errorf("encoding the node pool as yaml: %w", err)
	}
	_, _ = fmt.Fprintln(w)
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		_, _ = fmt.Fprintf(w, "  %s\n", line)
	}
	_, _ = fmt.Fprintln(w)
	return nil
}

// writePoolTable summarises the pool list as it would look after the edit.
func writePoolTable(w io.Writer, pools []vitiv1alpha1.KubernetesClusterNodePool) {
	_, _ = fmt.Fprintln(w, "   node pools after the change:")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(tw, "   NAME\tREPLICAS\tMACHINE CLASS")
	for i := range pools {
		name := pools[i].Name
		if name == "" {
			name = fmt.Sprintf("<unnamed pool #%d>", i)
		}
		_, _ = fmt.Fprintf(tw, "   %s\t%d\t%s\n", name, pools[i].Replicas, valueOrDash(pools[i].MachineClass))
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(w)
}

// apply writes the edit into the cluster spec. The delete re-finds the pool
// by name rather than trusting the index resolved earlier: the patch loop
// re-reads the object, and the list may have been reordered since.
func (e poolEdit) apply(kc *vitiv1alpha1.KubernetesCluster) error {
	pools := kc.Spec.Topology.Workers.NodePools
	switch e.kind {
	case poolEditAdd:
		kc.Spec.Topology.Workers.NodePools = append(pools, e.newPool)
		return nil
	case poolEditDelete:
		for i := range pools {
			if pools[i].Name != e.poolName {
				continue
			}
			kc.Spec.Topology.Workers.NodePools = append(pools[:i], pools[i+1:]...)
			return nil
		}
		return fmt.Errorf("node pool %q is no longer on this cluster — re-run the command", e.poolName)
	}
	return fmt.Errorf("unknown pool edit kind %d", e.kind)
}

// verifyPoolSetUnchanged checks that the pool set still supports the edit the
// operator confirmed — the companion of verifyTargetsUnchanged, and like it a
// semantic guard rather than a resourceVersion one: status churn is none of
// our business, a pool appearing or disappearing is.
func verifyPoolSetUnchanged(fresh *vitiv1alpha1.KubernetesCluster, edit poolEdit) error {
	pools := fresh.Spec.Topology.Workers.NodePools
	switch edit.kind {
	case poolEditAdd:
		for i := range pools {
			if pools[i].Name == edit.newPool.Name {
				return fmt.Errorf("a node pool named %q now exists on this cluster — re-run the command",
					edit.newPool.Name)
			}
		}
		return nil
	case poolEditDelete:
		// Re-checks existence and the last-pool guard against the fresh
		// object: a pool deleted by someone else while the prompt was open
		// must not leave this delete to take out the survivor.
		_, err := checkPoolDelete(pools, edit.poolName)
		return err
	}
	return fmt.Errorf("unknown pool edit kind %d", edit.kind)
}

// patchPoolEdit writes the approved edit, re-reading the cluster on each
// attempt — the same mechanics as patchScale: conflicts mean status churn
// raced the write and are retried; a pool set that genuinely changed aborts
// instead, and is not retried.
func patchPoolEdit(ctx context.Context, c ctrlclient.Client, key ctrlclient.ObjectKey, edit poolEdit) (*vitiv1alpha1.KubernetesCluster, error) {
	var out *vitiv1alpha1.KubernetesCluster
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &vitiv1alpha1.KubernetesCluster{}
		if err := c.Get(ctx, key, fresh); err != nil {
			return err
		}
		if err := verifyPoolSetUnchanged(fresh, edit); err != nil {
			// Not a conflict: retrying would re-read the same moved set.
			return err
		}
		base := fresh.DeepCopy()
		if err := edit.apply(fresh); err != nil {
			return err
		}
		// The lock is cheap here — the read is milliseconds old, so it guards
		// the write window rather than the operator's whole prompt. It is
		// also what makes the whole-array merge patch safe: a concurrent
		// nodePools write bumps the resourceVersion and this attempt retries
		// from a fresh read instead of overwriting it.
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

// confirmPoolEdit asks for consent proportional to the edit. Deleting a pool
// deletes its worker machines, so it takes the pool name typed back rather
// than a keystroke; adding one is a y/N.
func confirmPoolEdit(cmd *cobra.Command, edit poolEdit) error {
	if edit.kind == poolEditDelete {
		return confirmTypedName(cmd, "node pool",
			fmt.Sprintf("deleting node pool %q and its worker machines", edit.poolName), edit.poolName)
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

// runPoolEdit is the pool-edit half of the scale command's execution: plan,
// dry-run short-circuit, confirmation, patch — the same sequence the replica
// path runs, against the same cluster the plan was rendered from.
func runPoolEdit(ctx context.Context, cmd *cobra.Command, hit *kcHit, edit poolEdit) error {
	out := cmd.OutOrStdout()
	if err := writePoolEditPlan(out, hit.client.AZ.Name, hit.cluster.Namespace,
		hit.cluster.Name, edit, hit.cluster.Spec.Topology.Workers.NodePools); err != nil {
		return err
	}
	if kcScaleDryRun {
		_, _ = fmt.Fprintln(out, "🔍 dry run — nothing was changed")
		return nil
	}
	if !kcScaleYes {
		if err := confirmPoolEdit(cmd, edit); err != nil {
			return err
		}
	}
	key := ctrlclient.ObjectKeyFromObject(hit.cluster)
	if _, err := patchPoolEdit(ctx, hit.client.Ctrl, key, edit); err != nil {
		if apierrors.IsConflict(err) {
			return fmt.Errorf("kubernetescluster %s/%s is being rewritten faster than this command can patch it — re-run it: %w",
				hit.cluster.Namespace, hit.cluster.Name, err)
		}
		return fmt.Errorf("patching kubernetescluster %s/%s: %w", hit.cluster.Namespace, hit.cluster.Name, err)
	}
	_, _ = fmt.Fprintf(out, "✅ patched kubernetescluster/%s/%s\n", hit.cluster.Namespace, hit.cluster.Name)
	switch edit.kind {
	case poolEditAdd:
		_, _ = fmt.Fprintf(out, "   added node pool %q (%d replicas, machine class %q)\n",
			edit.newPool.Name, edit.newPool.Replicas, edit.newPool.MachineClass)
	case poolEditDelete:
		_, _ = fmt.Fprintf(out, "   removed node pool %q\n", edit.poolName)
	}
	_, _ = fmt.Fprintf(out, "   follow with: viti kc get %s\n", hit.cluster.Name)
	_, _ = fmt.Fprintf(out, "                viti kc health %s\n", hit.cluster.Name)
	return nil
}

func init() {
	kcScaleCmd.Flags().BoolVar(&kcScaleAddNodePool, "add-nodepool", false,
		"add a node pool cloned from an existing one")
	kcScaleCmd.Flags().StringVar(&kcScaleDeleteNodePool, "delete-nodepool", "",
		"delete the named node pool and its worker machines (requires typing the name back)")
	kcScaleCmd.Flags().StringVar(&kcScaleFromPool, "from-pool", "",
		"pool to clone with --add-nodepool (default: the only pool, or a picker)")
	kcScaleCmd.Flags().StringVar(&kcScaleMachineClass, "machineclass", "",
		"machine class for the new pool with --add-nodepool (default: a picker)")
	kcScaleCmd.Flags().StringVar(&kcScaleReplicas, "replicas", "",
		"replica count for the new pool with --add-nodepool, absolute and at least 1 (default: a prompt)")
	kcScaleCmd.Flags().StringVar(&kcScalePoolName, "name", "",
		"name for the new pool with --add-nodepool (default: generated, workers-<hex>)")
}
