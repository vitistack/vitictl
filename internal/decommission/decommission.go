// Package decommission implements full guest-cluster decommissioning:
//
//	Phase 1 (guest): clean up inside the guest cluster so external systems
//	  release their state — ArgoCD stopped, ingresses/gateways/LB services
//	  deleted (IPAM + DNS cleanup by their operators), PVCs deleted and
//	  waited on until the backing volumes are gone from external storage,
//	  and the cluster purged from the ROR registry.
//	Phase 2 (mgmt): delete the KubernetesCluster CR and watch the operator
//	  teardown (Machines/kubevirt VMs, NetworkConfigurations,
//	  ControlPlaneVirtualSharedIP, IPAllocations) until verifiably gone.
//
// The ordering is load-bearing: phase 1 must run while the guest is alive,
// because the external cleanup is performed by operators inside the guest
// and by its CSI drivers. Phase 2 runs only after phase 1's verdict is
// verifiably clean.
//
// Finalizers are never stripped anywhere: they ARE the cleanup mechanism.
//
// This is a Go port of the operations-drift scripts
// (scripts/clusterdelete/*.sh), which remain the validated reference.
package decommission

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

// Options configures a decommission run.
type Options struct {
	// Out receives human-readable progress output.
	Out io.Writer

	// RORAPIURL is the ROR API base URL. Empty = https://api.ror.nhn.no.
	RORAPIURL string
	// RORConfigPath is the ror CLI config holding the access token.
	// Empty = ~/.ror/config.yaml.
	RORConfigPath string

	// SkipPreclean skips phase 1 entirely (guest unreachable / already
	// precleaned). External state held by the guest will NOT be cleaned.
	SkipPreclean bool

	// MachineTimeout bounds the wait for VM teardown in phase 2.
	// Zero = 15 minutes.
	MachineTimeout time.Duration
}

func (o *Options) withDefaults() Options {
	out := *o
	if out.Out == nil {
		out.Out = io.Discard
	}
	if out.RORAPIURL == "" {
		out.RORAPIURL = "https://api.ror.nhn.no"
	}
	if out.MachineTimeout == 0 {
		out.MachineTimeout = 15 * time.Minute
	}
	return out
}

// Runner executes a decommission of one cluster.
type Runner struct {
	opts Options

	mgmt    ctrlclient.Client
	guest   ctrlclient.Client
	cluster *vitiv1alpha1.KubernetesCluster

	clusterID string
	namespace string

	// failed accumulates non-fatal problems; any true blocks the verdict.
	failed bool

	ror *rorPurge
}

// New builds a Runner. guest may be nil only when opts.SkipPreclean is set.
func New(mgmt, guest ctrlclient.Client, cluster *vitiv1alpha1.KubernetesCluster, opts Options) (*Runner, error) {
	if cluster.Spec.Cluster.ClusterId == "" {
		return nil, fmt.Errorf("cluster %s/%s has no clusterId — refusing to proceed", cluster.Namespace, cluster.Name)
	}
	o := opts.withDefaults()
	if guest == nil && !o.SkipPreclean {
		return nil, fmt.Errorf("no guest client and preclean not skipped")
	}
	return &Runner{
		opts:      o,
		mgmt:      mgmt,
		guest:     guest,
		cluster:   cluster,
		clusterID: cluster.Spec.Cluster.ClusterId,
		namespace: cluster.Namespace,
	}, nil
}

func (r *Runner) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.opts.Out, format+"\n", args...)
}

func (r *Runner) warnf(format string, args ...any) {
	r.printf("⚠️  WARNING: "+format, args...)
}

// failf records a verdict-blocking problem without aborting the run.
func (r *Runner) failf(format string, args ...any) {
	r.failed = true
	r.warnf(format, args...)
}

// waitUntil polls check every interval until it returns true, the timeout
// elapses, or ctx is done. Errors from check are transient (API blips) and
// must never be conflated with "resource absent": they are surfaced and the
// poll continues. Returns false on timeout.
func (r *Runner) waitUntil(ctx context.Context, desc string, timeout, interval time.Duration, check func(context.Context) (bool, error)) bool {
	deadline := time.Now().Add(timeout)
	start := time.Now()
	var lastErr string
	for {
		ok, err := check(ctx)
		if err != nil {
			if msg := err.Error(); msg != lastErr {
				r.printf("  (query error while waiting for %s — retrying: %v)", desc, err)
				lastErr = msg
			}
		} else if ok {
			r.printf("  %s: done after %s", desc, time.Since(start).Round(time.Second))
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			r.warnf("%s did not complete within %s", desc, timeout)
			return false
		}
		time.Sleep(interval)
	}
}

// ignoreNotFound maps IsNotFound to nil for delete calls.
func ignoreNotFound(err error) error {
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// hasClusterPrefix reports whether name belongs to this run's cluster. The
// clusterId carries a unique random suffix, so prefix matching is
// collision-safe against similarly named neighbors in the namespace.
func (r *Runner) hasClusterPrefix(name string) bool {
	return strings.HasPrefix(name, r.clusterID+"-")
}

// Run executes the full decommission. It returns an error when the run is
// NOT CLEAN — the caller should treat that as "do not consider this cluster
// gone; nothing further will be torn down automatically".
func (r *Runner) Run(ctx context.Context) error {
	if !r.opts.SkipPreclean {
		if err := r.preclean(ctx); err != nil {
			return err
		}
	} else {
		r.warnf("preclean SKIPPED — external state held by the guest (IPAM, DNS, volumes, ROR) is not cleaned up")
	}
	return r.teardown(ctx)
}

// RunPreclean executes only phase 1 (guest cleanup + ROR deregistration),
// leaving the KubernetesCluster and its VMs untouched — for decommissions
// where the irreversible deletion happens later (e.g. in a change window).
// Re-runnable: a clean verdict stays clean on repeat runs.
func (r *Runner) RunPreclean(ctx context.Context) error {
	if r.guest == nil {
		return fmt.Errorf("no guest client — preclean needs the guest cluster to be reachable")
	}
	return r.preclean(ctx)
}
