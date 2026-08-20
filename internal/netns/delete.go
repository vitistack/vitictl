package netns

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

// pollInterval is how often DeleteAndWait re-checks the object; a var so
// tests can shrink it.
var pollInterval = 3 * time.Second

func clientKey(nn *vitiv1alpha1.NetworkNamespace) ctrlclient.ObjectKey {
	return ctrlclient.ObjectKey{Namespace: nn.Namespace, Name: nn.Name}
}

// callCtx bounds ONE API call. The timeout parameter of DeleteAndWait is a
// wall-clock deadline checked BETWEEN polls, which is worth nothing if a
// single call never returns — and NetworkNamespace requests traverse a CRD
// conversion webhook that, on at least one availability zone, hangs forever
// instead of erroring.
//
// The bound is deliberately per attempt and NOT set on the outer ctx: a
// per-attempt expiry surfaces as an ordinary call error, falls through to the
// lastErr path, and still produces the "could not verify" verdict. A deadline
// on the outer ctx would instead return a bare context error and lose the
// NOT CLEAN / do-not-strip framing that the whole command exists to deliver.
//
// The budget is fixed rather than shrunk to the wall-clock time remaining.
// Shrinking looks tidier but corrupts the diagnosis: the last poll before the
// deadline (or every poll, when --timeout is set below normal API latency)
// would get a millisecond-scale budget, fail with DeadlineExceeded, set
// lastErr, and report "could not verify — the API kept erroring" for an
// object the API answered about perfectly well. That is the wrong verdict:
// a finalizer still held must read NOT CLEAN, which is the whole point of
// waiting. The cost of not shrinking is bounded and small — the wait can
// overshoot --timeout by at most one budget (2*pollInterval, 6s by default).
func callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 2*pollInterval)
}

// DeleteAndWait deletes one NetworkNamespace and waits until the operator has
// released the finalizer (= external NAM teardown done) and the object is
// gone. Finalizers are never touched: a held finalizer past the timeout is
// reported NOT CLEAN and points at the operator. A query error is never
// treated as absence.
//
// opts are passed to the Delete call — callers should pass
// ctrlclient.Preconditions{UID: &nn.UID} so a deleted-and-recreated object of
// the same name cannot be hit by mistake.
func DeleteAndWait(ctx context.Context, c ctrlclient.Client, nn *vitiv1alpha1.NetworkNamespace, timeout time.Duration, out io.Writer, opts ...ctrlclient.DeleteOption) error {
	deadline := time.Now().Add(timeout)

	if nn.DeletionTimestamp.IsZero() {
		dctx, cancel := callCtx(ctx)
		err := c.Delete(dctx, nn, opts...)
		cancel()
		switch {
		case apierrors.IsNotFound(err):
			_, _ = fmt.Fprintf(out, "✅ networknamespace %s/%s already gone\n", nn.Namespace, nn.Name)
			return nil
		case err != nil && errors.Is(dctx.Err(), context.DeadlineExceeded):
			return fmt.Errorf("the delete request for %s/%s did not come back in time — it may or may not have been accepted, "+
				"so external NAM state (VLAN %d, prefixes) may or may not be tearing down. Re-run to verify; "+
				"do NOT strip the finalizer. Check the CRD conversion webhook on this management cluster: %w",
				nn.Namespace, nn.Name, nn.Status.VlanID, err)
		case err != nil:
			return fmt.Errorf("deleting networknamespace %s/%s: %w", nn.Namespace, nn.Name, err)
		}
		_, _ = fmt.Fprintf(out, "🗑️  %s/%s: delete issued — waiting for the operator to release the finalizer (external NAM teardown: VLAN %d, prefixes)\n",
			nn.Namespace, nn.Name, nn.Status.VlanID)
	} else {
		_, _ = fmt.Fprintf(out, "⏳ %s/%s: already Terminating (deletionTimestamp set) — waiting for finalizer release, not re-deleting\n",
			nn.Namespace, nn.Name)
	}

	var lastErr error
	for {
		var cur vitiv1alpha1.NetworkNamespace
		gctx, cancel := callCtx(ctx)
		err := c.Get(gctx, clientKey(nn), &cur)
		cancel()
		switch {
		case apierrors.IsNotFound(err):
			_, _ = fmt.Fprintf(out, "✅ networknamespace %s/%s deleted and verified gone (VLAN %d and prefixes released via operator)\n",
				nn.Namespace, nn.Name, nn.Status.VlanID)
			return nil
		case err != nil:
			lastErr = err // query error ≠ absence: keep polling, report if it persists
		default:
			lastErr = nil
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			// Keep the operator-facing framing even when the caller's context
			// ends the wait: the delete was already issued, so the finalizer is
			// the thing to reason about — not the interruption.
			return fmt.Errorf("stopped waiting for %s/%s before the operator released the finalizer, "+
				"so external NAM state (VLAN %d, prefixes) may still be tearing down. Re-run to verify; "+
				"do NOT strip the finalizer: %w", nn.Namespace, nn.Name, nn.Status.VlanID, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("could not verify deletion of %s/%s within %s — the API kept erroring or not answering (last: %w); "+
			"the object may or may not be gone, re-run to verify. Do NOT strip the finalizer",
			nn.Namespace, nn.Name, timeout, lastErr)
	}
	return fmt.Errorf("NOT CLEAN: networknamespace %s/%s still present after %s — finalizer not released, so external NAM state (VLAN %d, prefixes) is NOT torn down. "+
		"do NOT strip the finalizer; investigate the networknamespace operator on this management cluster",
		nn.Namespace, nn.Name, timeout, nn.Status.VlanID)
}
