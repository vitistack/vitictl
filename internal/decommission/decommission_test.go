package decommission

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

// --- waitUntil ---------------------------------------------------------
//
// waitUntil is the wait primitive every teardown/preclean step is built on.
// A bug here silently propagates into every "cleared", "removed", "gone"
// check in the package, so it is tested in isolation with tiny timeouts.

// TestWaitUntilSucceedsImmediately: if a true,nil check is not honoured on
// the first poll, every "already clean" fast-path would instead sleep out a
// full timeout — slow but not unsafe, so this is a sanity check, not a
// safety property.
func TestWaitUntilSucceedsImmediately(t *testing.T) {
	r, out := newTestRunner(t, nil, nil)
	calls := 0
	ok := r.waitUntil(t.Context(), "thing", 50*time.Millisecond, time.Millisecond, func(context.Context) (bool, error) {
		calls++
		return true, nil
	})
	if !ok {
		t.Fatal("waitUntil should report success when check returns (true, nil)")
	}
	if calls != 1 {
		t.Errorf("expected exactly one check call, got %d", calls)
	}
	if !strings.Contains(out.String(), "done after") {
		t.Errorf("expected a completion line in output, got %q", out.String())
	}
}

// TestWaitUntilErrorNeverMeansSuccess is the single most important property
// in this file: if a query error were ever treated as "resource gone", the
// tool would report a cluster destroyed while its Machines/PVs/VIPs are
// still alive on the provider — silent state leak dressed up as success.
func TestWaitUntilErrorNeverMeansSuccess(t *testing.T) {
	r, _ := newTestRunner(t, nil, nil)
	calls := 0
	ok := r.waitUntil(t.Context(), "thing", 20*time.Millisecond, time.Millisecond, func(context.Context) (bool, error) {
		calls++
		return true, errors.New("transient api error") // ok=true is ignored because err != nil
	})
	if ok {
		t.Fatal("an error from check must never be treated as success, even when the bool is true")
	}
	if calls < 2 {
		t.Errorf("expected the poll to keep retrying past the first error, only saw %d calls", calls)
	}
}

// TestWaitUntilTimesOutAndWarns: on real timeout the caller must see a
// warning — this is what tells an operator "the tool didn't confirm this,
// go look" instead of silently moving on.
func TestWaitUntilTimesOutAndWarns(t *testing.T) {
	r, out := newTestRunner(t, nil, nil)
	ok := r.waitUntil(t.Context(), "never-ready", 10*time.Millisecond, time.Millisecond, func(context.Context) (bool, error) {
		return false, nil
	})
	if ok {
		t.Fatal("expected timeout (false) when check never succeeds")
	}
	if !strings.Contains(out.String(), "WARNING") || !strings.Contains(out.String(), "did not complete within") {
		t.Errorf("expected a timeout warning in output, got %q", out.String())
	}
}

// TestWaitUntilChecksAtLeastOnce: a zero/expired timeout must still look
// once before giving up. Without this, a caller passing an already-expired
// deadline would report "not done" without ever querying reality.
func TestWaitUntilChecksAtLeastOnce(t *testing.T) {
	r, _ := newTestRunner(t, nil, nil)
	calls := 0
	ok := r.waitUntil(t.Context(), "thing", 0, time.Millisecond, func(context.Context) (bool, error) {
		calls++
		return true, nil
	})
	if calls != 1 {
		t.Fatalf("expected exactly one check even with a zero timeout, got %d calls", calls)
	}
	if !ok {
		t.Fatal("the single check succeeded; waitUntil must report success rather than giving up unseen")
	}
}

// TestWaitUntilDedupsRepeatedError verifies the lastErr dedup logic: a
// flapping API error that repeats identically on every poll must be logged
// once, not once per poll — otherwise a 60s wait with a 1s interval spams 60
// near-identical lines and buries the one warning an operator needs to see.
func TestWaitUntilDedupsRepeatedError(t *testing.T) {
	r, out := newTestRunner(t, nil, nil)
	r.waitUntil(t.Context(), "thing", 15*time.Millisecond, 2*time.Millisecond, func(context.Context) (bool, error) {
		return false, errors.New("connection refused")
	})
	got := strings.Count(out.String(), "query error while waiting for thing")
	if got != 1 {
		t.Errorf("expected the repeated identical error to be printed exactly once, got %d occurrences in %q", got, out.String())
	}
}

// TestWaitUntilDedupAllowsChangedError: dedup must be keyed on the message,
// not "print once ever" — a genuinely new error after a different one must
// still surface, or a real problem could hide behind an earlier one.
func TestWaitUntilDedupAllowsChangedError(t *testing.T) {
	r, out := newTestRunner(t, nil, nil)
	calls := 0
	r.waitUntil(t.Context(), "thing", 15*time.Millisecond, 2*time.Millisecond, func(context.Context) (bool, error) {
		calls++
		if calls <= 2 {
			return false, errors.New("error A")
		}
		return false, errors.New("error B")
	})
	out2 := out.String()
	if strings.Count(out2, "error A") != 1 {
		t.Errorf("expected error A logged once, got: %q", out2)
	}
	if strings.Count(out2, "error B") == 0 {
		t.Errorf("expected error B (a change from error A) to surface, got: %q", out2)
	}
}

// TestWaitUntilContextCancelledEndsWait: a cancelled context must cut the
// wait short rather than run out the full timeout — otherwise Ctrl-C during
// a decommission would hang for up to the longest configured timeout (up to
// 15 minutes) instead of returning promptly.
func TestWaitUntilContextCancelledEndsWait(t *testing.T) {
	r, _ := newTestRunner(t, nil, nil)
	ctx, cancel := ctxWithCancel(t)
	cancel() // already cancelled before the first poll

	start := time.Now()
	ok := r.waitUntil(ctx, "thing", 5*time.Second, time.Millisecond, func(context.Context) (bool, error) {
		return false, nil
	})
	elapsed := time.Since(start)
	if ok {
		t.Fatal("a cancelled context must not be reported as success")
	}
	if elapsed > time.Second {
		t.Errorf("waitUntil should end promptly on a cancelled context, took %s against a 5s timeout", elapsed)
	}
}

// --- hasClusterPrefix ----------------------------------------------------
//
// hasClusterPrefix decides which Machines/NetworkConfigurations are "ours"
// for both counting (verifyTeardown) and deletion purposes elsewhere in the
// package. Getting it wrong means touching, or worse reporting deleted, a
// neighbouring cluster's resources.

func TestHasClusterPrefixMatchesOwnResource(t *testing.T) {
	r, _ := newTestRunner(t, nil, nil) // clusterID = testClusterID = "t-team-001-ab12"
	if !r.hasClusterPrefix(testClusterID + "-ctp0") {
		t.Fatal("a name of the form <clusterID>-<suffix> must be recognised as belonging to this cluster")
	}
}

// TestHasClusterPrefixRejectsBareClusterID: the bare clusterID with no
// suffix is never a valid Machine/NetworkConfiguration name in this scheme;
// treating it as a match would be a modelling error, not a real object.
func TestHasClusterPrefixRejectsBareClusterID(t *testing.T) {
	r, _ := newTestRunner(t, nil, nil)
	if r.hasClusterPrefix(testClusterID) {
		t.Fatal("the bare clusterID (no trailing dash + suffix) must not match")
	}
}

// TestHasClusterPrefixRejectsSharedStringPrefix is the collision-safety
// property the doc comment promises: a neighbour cluster whose id merely
// starts with the same characters (e.g. "t-team-001-ab12x") must NOT match
// "t-team-001-ab12"'s prefix check. If it did, this runner would delete a
// different, still-live cluster's Machines.
func TestHasClusterPrefixRejectsSharedStringPrefix(t *testing.T) {
	r, _ := newTestRunner(t, nil, nil) // clusterID = "t-team-001-ab12"
	neighbourMachine := testClusterID + "x-ctp0"
	if r.hasClusterPrefix(neighbourMachine) {
		t.Fatalf("name %q must not match clusterID %q — it belongs to a different cluster whose id merely shares a string prefix; the trailing dash is what must prevent this", neighbourMachine, testClusterID)
	}
}

// TestHasClusterPrefixNestedClusterIDCurrentBehaviour pins the CURRENT
// behaviour for the nested case, not a wished-for one: a runner for cluster
// "t-a" is a strict string-prefix (plus dash) of a resource that actually
// belongs to a distinct cluster "t-a-b" (e.g. Machine "t-a-b-ctp0"). Because
// hasClusterPrefix only checks strings.HasPrefix(name, clusterID+"-"), the
// "t-a" runner WILL match "t-a-b-ctp0" and would count/delete it as its own.
// This is a real collision risk whenever one cluster's id is itself a
// dash-joined prefix of another's — flagged in the report, not fixed here.
func TestHasClusterPrefixNestedClusterIDCurrentBehaviour(t *testing.T) {
	r, _ := newTestRunner(t, nil, nil)
	r.clusterID = "t-a"
	nestedNeighbourMachine := "t-a-b-ctp0" // actually belongs to cluster "t-a-b"
	if !r.hasClusterPrefix(nestedNeighbourMachine) {
		t.Fatal("documenting current behaviour: hasClusterPrefix(\"t-a-b-ctp0\") for clusterID \"t-a\" is currently true (collision risk); if this ever flips to false, update the comment describing the risk")
	}
}

// --- New -----------------------------------------------------------------

// TestNewRefusesEmptyClusterID: clusterId drives hasClusterPrefix, the
// Machine/NetworkConfiguration deletion filter, and the cpvip/IPAllocation
// lookups by name. An empty clusterId would match everything (empty-string
// prefix) or nothing meaningful — New must refuse outright rather than let
// a Runner exist in that state.
func TestNewRefusesEmptyClusterID(t *testing.T) {
	kc := testKC()
	kc.Spec.Cluster.ClusterId = ""
	_, err := New(nil, nil, kc, Options{SkipPreclean: true})
	if err == nil {
		t.Fatal("expected New to refuse a cluster with no clusterId")
	}
	if !strings.Contains(err.Error(), "refus") {
		t.Errorf("error should explicitly say it is refusing to proceed, got: %v", err)
	}
}

// TestNewRefusesNilGuestWithoutSkipPreclean: without a guest client, phase 1
// (the only phase that cleans up external IPAM/DNS/volume state) cannot
// run. New must refuse construction rather than produce a Runner whose
// preclean will nil-pointer-panic or silently do nothing.
func TestNewRefusesNilGuestWithoutSkipPreclean(t *testing.T) {
	_, err := New(nil, nil, testKC(), Options{})
	if err == nil {
		t.Fatal("expected New to refuse a nil guest client when SkipPreclean is not set")
	}
}

// TestNewAcceptsNilGuestWithSkipPreclean: the one legitimate case for a nil
// guest — the operator has explicitly opted out of phase 1.
func TestNewAcceptsNilGuestWithSkipPreclean(t *testing.T) {
	r, err := New(nil, nil, testKC(), Options{SkipPreclean: true})
	if err != nil {
		t.Fatalf("expected New to accept a nil guest client when SkipPreclean is set, got: %v", err)
	}
	if r == nil {
		t.Fatal("expected a non-nil Runner")
	}
}

// TestNewPopulatesIdentityFromCR: clusterID and namespace are read off the
// CR at construction time — every prefix/namespace-scoped check downstream
// depends on these being copied correctly, not left zero.
func TestNewPopulatesIdentityFromCR(t *testing.T) {
	kc := testKC()
	r, err := New(nil, nil, kc, Options{SkipPreclean: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.clusterID != testClusterID {
		t.Errorf("clusterID = %q, want %q", r.clusterID, testClusterID)
	}
	if r.namespace != testNS {
		t.Errorf("namespace = %q, want %q", r.namespace, testNS)
	}
}

// --- withDefaults ----------------------------------------------------------

// TestWithDefaultsNilOutBecomesDiscard: printf does an unconditional
// fmt.Fprintf(r.opts.Out, ...) on every call. A nil Out would panic the
// very first time any step prints — which is every real run, since preclean
// and teardown both print constantly.
func TestWithDefaultsNilOutBecomesDiscard(t *testing.T) {
	r, err := New(nil, nil, testKC(), Options{SkipPreclean: true, Out: nil})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.opts.Out != io.Discard {
		t.Fatal("a nil Out must be defaulted to io.Discard")
	}
	// Prove it in practice too: this must not panic.
	r.printf("no panic please")
	r.warnf("still no panic")
}

// TestWithDefaultsMachineTimeout covers both defaulting branches directly on
// Options.withDefaults, without going through New.
func TestWithDefaultsMachineTimeout(t *testing.T) {
	zero := Options{}
	got := zero.withDefaults()
	if got.MachineTimeout != 15*time.Minute {
		t.Errorf("zero MachineTimeout should default to 15m, got %s", got.MachineTimeout)
	}

	custom := Options{MachineTimeout: 5 * time.Second}
	got2 := custom.withDefaults()
	if got2.MachineTimeout != 5*time.Second {
		t.Errorf("a non-zero MachineTimeout must be preserved, got %s", got2.MachineTimeout)
	}
}

// --- ignoreNotFound --------------------------------------------------------

// TestIgnoreNotFound: delete calls throughout the package rely on this to
// treat "already gone" as success while still surfacing real API errors
// (permissions, connectivity) that must block the verdict.
func TestIgnoreNotFound(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "pods"}, "some-name")
	other := errors.New("connection refused")

	tests := []struct {
		name string
		in   error
		want error
	}{
		{"nil stays nil", nil, nil},
		{"NotFound becomes nil", notFound, nil},
		{"other error passes through unchanged", other, other},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ignoreNotFound(tt.in)
			if !errors.Is(got, tt.want) && got != tt.want {
				t.Errorf("ignoreNotFound(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// --- Run ordering: the load-bearing property ------------------------------

// alwaysFailList is an interceptor.Funcs.List that fails every List call,
// simulating an unreachable/misbehaving guest API server during phase 1.
func alwaysFailList(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
	return errors.New("simulated guest API failure")
}

// TestRunDoesNotTeardownWhenPrecleanFails is THE property this package
// exists to guarantee: if phase 1 cannot verify the guest is clean, phase 2
// (deleting the KubernetesCluster CR, which starts irreversible VM teardown)
// must never run. If this regressed, Run would delete real VMs while their
// IPAM/DNS/volume state is still held by a guest cluster the tool never
// actually cleaned.
func TestRunDoesNotTeardownWhenPrecleanFails(t *testing.T) {
	sch := testScheme(t, false, false)

	guestBase := fake.NewClientBuilder().WithScheme(sch).Build()
	guest := interceptor.NewClient(guestBase, interceptor.Funcs{List: alwaysFailList})

	kc := testKC()
	mgmt := fakeClient(sch, kc)

	r, out := newTestRunner(t, mgmt, guest)

	err := r.Run(t.Context())
	if err == nil {
		t.Fatal("expected Run to return an error when preclean cannot verify a clean guest")
	}

	// The load-bearing assertion: the CR must still be present, i.e.
	// teardown's mgmt.Delete was never reached.
	var got vitiv1alpha1.KubernetesCluster
	getErr := mgmt.Get(t.Context(), ctrlclient.ObjectKey{Namespace: testNS, Name: testCluster}, &got)
	if getErr != nil {
		t.Fatalf("KubernetesCluster CR should still exist after a failed preclean (phase 2 must not have started), Get error: %v", getErr)
	}
	if !strings.Contains(out.String(), "Phase 1") {
		t.Errorf("expected phase 1 to have been announced in output, got: %q", out.String())
	}
	if strings.Contains(out.String(), "Phase 2") {
		t.Errorf("phase 2 must never be announced when preclean fails, got: %q", out.String())
	}
}

// TestRunSkipPrecleanWarnsAndProceeds: SkipPreclean is an explicit,
// deliberate operator override — it must warn loudly (so the decision is
// visible in logs/output) and then actually proceed into phase 2, rather
// than silently doing nothing.
func TestRunSkipPrecleanWarnsAndProceeds(t *testing.T) {
	sch := testScheme(t, false, false)
	kc := testKC()
	mgmt := fakeClient(sch, kc)

	r, out := newTestRunner(t, mgmt, nil)
	r.opts.SkipPreclean = true
	r.opts.MachineTimeout = 20 * time.Millisecond

	err := r.Run(t.Context())

	got := out.String()
	if !strings.Contains(got, "WARNING") || !strings.Contains(got, "preclean SKIPPED") {
		t.Errorf("expected a loud preclean-skipped warning in output, got: %q", got)
	}
	if !strings.Contains(got, "Phase 2") {
		t.Errorf("expected teardown (phase 2) to have been entered, got: %q", got)
	}
	// With no leftover Machines/NetworkConfigurations/cpvip and the CR
	// deleted, teardown's verdict should be clean.
	if err != nil {
		t.Fatalf("expected a clean teardown verdict with no leftover resources, got error: %v", err)
	}
	var deleted vitiv1alpha1.KubernetesCluster
	getErr := mgmt.Get(t.Context(), ctrlclient.ObjectKey{Namespace: testNS, Name: testCluster}, &deleted)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected the KubernetesCluster CR to have been deleted by teardown, Get error: %v", getErr)
	}
}
