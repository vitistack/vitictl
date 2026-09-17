package decommission

import (
	"context"
	"sort"
	"strings"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// webhookSweep is the outcome of one removal pass.
type webhookSweep struct {
	// Gone are the configurations verified absent after the pass.
	Gone []string
	// Remaining are the configurations still present afterwards — a delete
	// the API accepted but that has not taken effect, usually a finalizer
	// holding the object in Terminating. The API server keeps consulting a
	// webhook configuration until it actually leaves storage, so these still
	// reject writes and must not be reported as removed.
	Remaining []string
}

// deleteAdmissionWebhooks removes every validating and mutating webhook
// configuration from the guest cluster and reports what actually went.
//
// Unconditional and unprompted, on purpose. An admission webhook whose
// backend is not answering rejects EVERY write the API server routes to it —
// failurePolicy defaults to Fail — and a cluster queued for decommissioning
// is exactly where a policy controller gets left for dead. On the run that
// prompted this code, Kyverno's pods sat in Completed with its admission
// pods gone, so every patch and delete in phase 1 came back "failed calling
// webhook". Phase 1 is nothing but writes, so one stale webhook
// configuration turns the whole phase into manual labour. The same failure
// class hit an earlier decommission through ipam-operator's webhook, where
// it made LoadBalancer services undeletable.
//
// Removing them is also nowhere near the most destructive thing this phase
// does: the same phase deletes every PVC and every PV in the cluster.
// Admission policy protects a cluster with a future; this one has none, so
// there is nothing left to protect and nobody to ask. Note that phase 1 is
// also reachable on its own through RunPreclean, which leaves the guest
// running until a later change window — see its doc comment.
//
// Called twice — before the first write, and again once ArgoCD is stopped,
// because Argo puts back anything it manages while its controllers run.
func (r *Runner) deleteAdmissionWebhooks(ctx context.Context) webhookSweep {
	before, err := r.listAdmissionWebhooks(ctx)
	if err != nil {
		r.warnf("could not list admission webhook configurations: %v", err)
		return webhookSweep{}
	}
	if len(before) == 0 {
		return webhookSweep{}
	}

	for _, key := range sortedKeys(before) {
		if err := ignoreNotFound(r.guest.Delete(ctx, before[key])); err != nil {
			r.warnf("failed to delete %s: %v", key, err)
			if isWebhookError(err) {
				r.warnf("  that deletion was itself rejected by an admission webhook — nothing after this will succeed until it is removed by hand")
			}
		}
	}

	// A delete the API accepted is not a webhook that has stopped being
	// consulted. Re-read rather than claim.
	after, err := r.listAdmissionWebhooks(ctx)
	if err != nil {
		r.warnf("could not verify admission webhook removal: %v — treating them as still present", err)
		return webhookSweep{Remaining: sortedKeys(before)}
	}

	var sweep webhookSweep
	for _, key := range sortedKeys(before) {
		if _, still := after[key]; still {
			sweep.Remaining = append(sweep.Remaining, key)
			continue
		}
		sweep.Gone = append(sweep.Gone, key)
		r.printf("  Deleted %s", key)
	}
	for _, key := range sweep.Remaining {
		r.warnf("%s is still present after an accepted delete — a finalizer is holding it, and it keeps rejecting writes until it is gone", key)
	}
	return sweep
}

// listAdmissionWebhooks returns every validating and mutating webhook
// configuration, keyed "kind/name" so the same key identifies an object
// across the before and after reads.
func (r *Runner) listAdmissionWebhooks(ctx context.Context) (map[string]ctrlclient.Object, error) {
	found := map[string]ctrlclient.Object{}

	var vals admissionv1.ValidatingWebhookConfigurationList
	if err := r.guest.List(ctx, &vals); err != nil {
		return nil, err
	}
	for i := range vals.Items {
		found["validatingwebhookconfiguration/"+vals.Items[i].Name] = &vals.Items[i]
	}

	var muts admissionv1.MutatingWebhookConfigurationList
	if err := r.guest.List(ctx, &muts); err != nil {
		return nil, err
	}
	for i := range muts.Items {
		found["mutatingwebhookconfiguration/"+muts.Items[i].Name] = &muts.Items[i]
	}

	return found, nil
}

// reappeared counts the configurations in a later sweep that the earlier one
// had genuinely removed — the ones something put back.
//
// Worth separating from those the earlier pass never managed to remove,
// because the two need opposite responses and attributing a stuck finalizer
// to an ArgoCD re-creation sends the reader to the wrong place.
func reappeared(later, earlier webhookSweep) int {
	stuck := map[string]bool{}
	for _, key := range earlier.Remaining {
		stuck[key] = true
	}
	n := 0
	for _, key := range append(append([]string{}, later.Gone...), later.Remaining...) {
		if !stuck[key] {
			n++
		}
	}
	return n
}

func sortedKeys(m map[string]ctrlclient.Object) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// isWebhookError reports whether an API error is the API server failing to
// reach an admission webhook, rather than rejecting the request on its
// merits. The distinction is worth drawing because the two need opposite
// responses: a rejection means the request was wrong, an unreachable webhook
// means nothing will be accepted at all until its configuration is gone.
//
// Matched on the message because the API server returns this as a generic
// InternalError with the webhook named only in the text.
func isWebhookError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "failed calling webhook")
}
