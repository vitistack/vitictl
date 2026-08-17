package netns

import (
	"context"
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

// DeleteAndWait deletes one NetworkNamespace and waits until the operator has
// released the finalizer (= external NAM teardown done) and the object is
// gone. Finalizers are never touched: a held finalizer past the timeout is
// reported NOT CLEAN and points at the operator. A query error is never
// treated as absence.
func DeleteAndWait(ctx context.Context, c ctrlclient.Client, nn *vitiv1alpha1.NetworkNamespace, timeout time.Duration, out io.Writer) error {
	if nn.DeletionTimestamp.IsZero() {
		if err := c.Delete(ctx, nn); err != nil {
			if apierrors.IsNotFound(err) {
				_, _ = fmt.Fprintf(out, "✅ networknamespace %s/%s already gone\n", nn.Namespace, nn.Name)
				return nil
			}
			return fmt.Errorf("deleting networknamespace %s/%s: %w", nn.Namespace, nn.Name, err)
		}
		_, _ = fmt.Fprintf(out, "🗑️  delete issued — waiting for the operator to release the finalizer (external NAM teardown: VLAN %d, prefixes)\n", nn.Status.VlanID)
	} else {
		_, _ = fmt.Fprintf(out, "⏳ already Terminating (deletionTimestamp set) — waiting for finalizer release, not re-deleting\n")
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		var cur vitiv1alpha1.NetworkNamespace
		err := c.Get(ctx, clientKey(nn), &cur)
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
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("could not verify deletion of %s/%s within %s — the API kept erroring (last: %v); the object may or may not be gone, re-run to verify",
			nn.Namespace, nn.Name, timeout, lastErr)
	}
	return fmt.Errorf("NOT CLEAN: networknamespace %s/%s still present after %s — finalizer not released, so external NAM state (VLAN %d, prefixes) is NOT torn down. "+
		"do NOT strip the finalizer; investigate the networknamespace operator on this management cluster",
		nn.Namespace, nn.Name, timeout, nn.Status.VlanID)
}
