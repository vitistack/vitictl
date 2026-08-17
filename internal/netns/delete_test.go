package netns

import (
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
