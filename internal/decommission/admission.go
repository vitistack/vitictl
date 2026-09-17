package decommission

import (
	"context"
	"strings"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// deleteAdmissionWebhooks removes every validating and mutating webhook
// configuration from the guest cluster and returns how many it deleted.
//
// Unconditional and unprompted, on purpose. An admission webhook whose
// backend is not answering rejects EVERY write the API server routes to it —
// failurePolicy defaults to Fail — and a cluster queued for decommissioning
// is exactly where a policy controller gets left for dead. On the run that
// prompted this code, Kyverno's pods sat in Completed with its admission
// pods gone, so every patch and delete in phase 1 came back "failed calling
// webhook". Phase 1 is nothing but writes, so one stale webhook
// configuration turns the whole phase into manual labour.
//
// Removing them is also nowhere near the most destructive thing this phase
// does: the same phase deletes every PVC and every PV in the cluster.
// Admission policy protects a cluster with a future; this one has none, so
// there is nothing left to protect and nobody to ask.
//
// Called twice — before the first write, and again once ArgoCD is stopped,
// because Argo puts back anything it manages while its controllers run.
func (r *Runner) deleteAdmissionWebhooks(ctx context.Context) int {
	deleted := 0

	var vals admissionv1.ValidatingWebhookConfigurationList
	if err := r.guest.List(ctx, &vals); err != nil {
		r.warnf("could not list validating webhook configurations: %v", err)
	} else {
		for i := range vals.Items {
			if r.deleteWebhookConfig(ctx, &vals.Items[i], "validatingwebhookconfiguration") {
				deleted++
			}
		}
	}

	var muts admissionv1.MutatingWebhookConfigurationList
	if err := r.guest.List(ctx, &muts); err != nil {
		r.warnf("could not list mutating webhook configurations: %v", err)
	} else {
		for i := range muts.Items {
			if r.deleteWebhookConfig(ctx, &muts.Items[i], "mutatingwebhookconfiguration") {
				deleted++
			}
		}
	}

	return deleted
}

// deleteWebhookConfig deletes one configuration, reporting whether it went.
//
// A failure here is warned, never failed: this step exists to unblock the
// writes that follow, not as an end in itself, and a webhook configuration
// left behind in a cluster about to be deleted harms nothing on its own. If
// it really was blocking, the steps after it fail loudly and those failures
// are what block the verdict.
func (r *Runner) deleteWebhookConfig(ctx context.Context, obj ctrlclient.Object, kind string) bool {
	if err := ignoreNotFound(r.guest.Delete(ctx, obj)); err != nil {
		r.warnf("failed to delete %s %s: %v", kind, obj.GetName(), err)
		if isWebhookError(err) {
			r.warnf("  that deletion was itself rejected by an admission webhook — nothing after this will succeed until it is removed by hand")
		}
		return false
	}
	r.printf("  Deleted %s %s", kind, obj.GetName())
	return true
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
