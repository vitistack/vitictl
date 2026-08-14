package decommission

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// The ROR registry is NHN-specific, so its purge lives in the viti-nhn
// plugin (vitistack/vitictl-nhn, `viti nhn ror purge`) rather than in this
// generic codebase. Core keeps the guest-side half — stopping the ROR agents
// and reading the cluster's identity from its own secret — and delegates the
// registry deletion to the plugin binary.
//
// EnvNHNBinary overrides which binary is executed (tests, dev builds);
// otherwise viti-nhn is resolved from PATH, the same discovery vitictl's
// plugin dispatch uses.
const EnvNHNBinary = "VITI_NHN_BIN"

func nhnBinary() (string, error) {
	if p := os.Getenv(EnvNHNBinary); p != "" {
		return p, nil
	}
	return exec.LookPath("viti-nhn")
}

// rorPurge tracks the background ROR deregistration. It starts the moment
// the ROR agents are scaled down so the registry's inactivity guard counts
// down while the rest of the preclean runs; the result is collected before
// the phase-1 verdict.
type rorPurge struct {
	clusterID string
	done      chan struct{}
	ok        bool
	log       []string
}

// startRORPurge reads the cluster's ROR identity from its own secret, scales
// the ROR agents to zero, and launches the plugin-delegated purge in the
// background.
func (r *Runner) startRORPurge(ctx context.Context) {
	var ns corev1.Namespace
	if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Name: "nhn-ror"}, &ns); err != nil {
		r.printf("No nhn-ror namespace found, skipping ROR steps")
		return
	}
	r.printf("Stop ROR cluster agents (starts the ROR inactivity clock)")

	var secret corev1.Secret
	if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Namespace: "nhn-ror", Name: "ror-apikey"}, &secret); err != nil {
		r.warnf("could not read secret nhn-ror/ror-apikey — ROR purge will be skipped: %v", err)
		return
	}
	clusterID := string(secret.Data["CLUSTER_ID"])

	var deps appsv1.DeploymentList
	if err := r.guest.List(ctx, &deps, ctrlclient.InNamespace("nhn-ror")); err == nil {
		for i := range deps.Items {
			if err := r.scaleTo(ctx, &deps.Items[i], 0); err != nil {
				r.warnf("failed to scale down %s: %v", deps.Items[i].Name, err)
			}
		}
	}

	if clusterID == "" {
		r.failf("secret nhn-ror/ror-apikey has no CLUSTER_ID — ROR purge skipped")
		return
	}
	bin, err := nhnBinary()
	if err != nil {
		// This cluster is ROR-registered, so skipping deregistration would
		// leak a registry entry: block the clean verdict rather than let a
		// missing plugin pass silently.
		r.failf("viti-nhn plugin not found — ROR deregistration cannot run.")
		r.printf("  Install it (viti plugin install nhn), or purge manually: viti nhn ror purge %s --wait", clusterID)
		return
	}

	p := &rorPurge{clusterID: clusterID, done: make(chan struct{})}
	r.ror = p
	r.printf("  ROR identity from secret nhn-ror/ror-apikey: clusterid=%s", clusterID)
	r.printf("  Starting ROR purge in the background (delegated to %s)", bin)
	go r.runRORPurge(ctx, p, bin)
}

func (p *rorPurge) logf(format string, args ...any) {
	p.log = append(p.log, fmt.Sprintf(format, args...))
}

// runRORPurge waits for the agent pods to actually terminate, then executes
// the plugin: --force is safe precisely because the pods were just verified
// gone (a stronger liveness check than the registry's heartbeat heuristic);
// --wait covers older ror-api versions that ignore force; --yes because the
// operator confirmed the decommission at the top of the run. The plugin owns
// auth (its API-key config) and the not-found-is-done idempotency.
func (r *Runner) runRORPurge(ctx context.Context, p *rorPurge, bin string) {
	defer close(p.done)

	verifiedGone := false
	for range 12 {
		var pods corev1.PodList
		if err := r.guest.List(ctx, &pods, ctrlclient.InNamespace("nhn-ror")); err == nil && len(pods.Items) == 0 {
			verifiedGone = true
			break
		}
		select {
		case <-ctx.Done():
			p.logf("WARNING: context cancelled while waiting for ROR agent pods")
			return
		case <-time.After(5 * time.Second):
		}
	}
	if verifiedGone {
		p.logf("ROR agent pods confirmed gone — purging with force")
	} else {
		p.logf("WARNING: pods still present in nhn-ror — purging without force (inactivity guard applies)")
	}
	p.exec(ctx, bin, verifiedGone)
}

// exec runs the plugin purge and captures its combined output line-by-line
// into the log; p.ok is set only on a zero exit.
func (p *rorPurge) exec(ctx context.Context, bin string, force bool) {
	args := []string{"ror", "purge", p.clusterID, "--yes", "--wait"}
	if force {
		args = append(args, "--force")
	}
	// #nosec G204 -- launching the user-installed viti-nhn plugin binary,
	// resolved via PATH or the operator's own VITI_NHN_BIN; args are
	// program-constructed (same trust model as internal/plugin dispatch).
	cmd := exec.CommandContext(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		p.logf("WARNING: starting %s: %v", bin, err)
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		p.logf("WARNING: starting %s: %v", bin, err)
		return
	}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		p.logf("%s", scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		p.logf("WARNING: reading %s output: %v", bin, err)
	}
	if err := cmd.Wait(); err != nil {
		p.logf("WARNING: ROR purge failed: %v", err)
		return
	}
	p.ok = true
}

// collectRORResult waits for the background purge and folds its log and
// verdict into the run.
func (r *Runner) collectRORResult(ctx context.Context) {
	if r.ror == nil {
		return
	}
	r.printf("Deregister cluster from ROR (via viti-nhn plugin)")
	select {
	case <-r.ror.done:
	case <-time.After(25 * time.Minute):
		r.failf("ROR purge did not finish in time")
		return
	case <-ctx.Done():
		r.failf("context cancelled while waiting for ROR purge")
		return
	}
	for _, line := range r.ror.log {
		r.printf("  %s", line)
	}
	if !r.ror.ok {
		r.failed = true
	}
}
