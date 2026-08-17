package netns

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestDeleteAndWaitGone(t *testing.T) {
	// No finalizer → the fake client removes the object synchronously.
	target := nn("team-a", "team-a-x1", 2100)
	c := fake.NewClientBuilder().WithScheme(newScheme(t, false)).WithObjects(target).Build()

	var buf strings.Builder
	if err := DeleteAndWait(t.Context(), c, target, 5*time.Second, &buf); err != nil {
		t.Fatalf("DeleteAndWait: %v", err)
	}
}

func TestDeleteAndWaitTimeoutNeverStrips(t *testing.T) {
	// Finalizer held → the fake client parks the object in Terminating
	// forever, exactly like a dead operator. Must time out NOT CLEAN and the
	// object must still carry its finalizer afterwards.
	target := nn("team-a", "team-a-x1", 2100)
	target.Finalizers = []string{"networknamespace.vitistack.io/finalizer"}
	c := fake.NewClientBuilder().WithScheme(newScheme(t, false)).WithObjects(target).Build()

	saved := pollInterval
	pollInterval = 10 * time.Millisecond
	defer func() { pollInterval = saved }()

	var buf strings.Builder
	err := DeleteAndWait(t.Context(), c, target, 50*time.Millisecond, &buf)
	if err == nil {
		t.Fatal("want NOT CLEAN error on timeout")
	}
	if !strings.Contains(err.Error(), "NOT CLEAN") || !strings.Contains(err.Error(), "do NOT strip") {
		t.Errorf("timeout error must say NOT CLEAN and forbid stripping, got: %v", err)
	}

	got := nn("team-a", "team-a-x1", 0)
	if err := c.Get(t.Context(), clientKey(target), got); err != nil {
		t.Fatalf("object should still exist: %v", err)
	}
	if len(got.Finalizers) != 1 {
		t.Error("finalizer must never be touched")
	}
}

func TestDeleteAndWaitAlreadyTerminating(t *testing.T) {
	// deletionTimestamp already set → no second Delete call, just the wait.
	// With a finalizer and a tiny timeout this exercises the wait path; the
	// output must say it is already Terminating.
	target := nn("team-a", "team-a-x1", 2100)
	target.Finalizers = []string{"networknamespace.vitistack.io/finalizer"}
	c := fake.NewClientBuilder().WithScheme(newScheme(t, false)).WithObjects(target).Build()
	if err := c.Delete(t.Context(), target); err != nil { // puts it into Terminating
		t.Fatal(err)
	}

	saved := pollInterval
	pollInterval = 10 * time.Millisecond
	defer func() { pollInterval = saved }()

	fresh := nn("team-a", "team-a-x1", 0)
	if err := c.Get(t.Context(), clientKey(target), fresh); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	err := DeleteAndWait(t.Context(), c, fresh, 50*time.Millisecond, &buf)
	if err == nil {
		t.Fatal("want timeout (finalizer held)")
	}
	if !strings.Contains(buf.String(), "already Terminating") {
		t.Errorf("output should mention already Terminating, got: %q", buf.String())
	}
}

// TestDeleteAndWaitQueryErrorIsNotAbsence pins the second load-bearing safety
// property: a persistently-erroring Get must never be read as "gone". The
// initial Delete is allowed to succeed for real (finalizer held, so the fake
// client parks the object in Terminating rather than removing it); only the
// subsequent polling Get calls are intercepted to fail every time, as if the
// API server itself were down. The returned error must say "could not
// verify" (the query-error verdict) and must NOT say "NOT CLEAN" (the
// gone-vs-still-present verdict, which was never legitimately reached here
// because the object's actual state was never observed).
func TestDeleteAndWaitQueryErrorIsNotAbsence(t *testing.T) {
	target := nn("team-a", "team-a-x1", 2100)
	target.Finalizers = []string{"networknamespace.vitistack.io/finalizer"}

	getErr := apierrors.NewServiceUnavailable("boom")
	c := fake.NewClientBuilder().WithScheme(newScheme(t, false)).WithObjects(target).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
				return getErr
			},
		}).Build()

	saved := pollInterval
	pollInterval = 10 * time.Millisecond
	defer func() { pollInterval = saved }()

	var buf strings.Builder
	err := DeleteAndWait(t.Context(), c, target, 50*time.Millisecond, &buf)
	if err == nil {
		t.Fatal("want a non-nil error: a persistently-erroring Get must never be reported as verified gone")
	}
	if !strings.Contains(err.Error(), "could not verify") {
		t.Errorf("query-error verdict must say 'could not verify', got: %v", err)
	}
	if strings.Contains(err.Error(), "NOT CLEAN") {
		t.Errorf("query-error verdict must NOT say 'NOT CLEAN' — the object's existence was never established, got: %v", err)
	}
}
