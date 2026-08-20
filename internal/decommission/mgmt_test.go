package decommission

// Tests for internal/decommission/mgmt.go — phase 2 of decommissioning: delete
// the KubernetesCluster CR and verify the operator has actually torn down the
// real VMs, NetworkConfigurations, the API VIP, and node-IP allocations. This
// is the most destructive code in the tool: a false "clean" verdict here
// tells an operator it is safe to strip finalizers and reclaim a namespace
// that still has live infrastructure attached to it.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
)

// ipAllocation returns an unstructured IPAllocation in the test namespace,
// the shape the runner queries when the static-ip-operator's CRD is installed.
func ipAllocation(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	gvk := kube.IPAllocationListGVK.GroupVersion().WithKind("IPAllocation")
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(testNS)
	u.SetName(name)
	return u
}

// ---------------------------------------------------------------------------
// verifyTeardown — must never issue a false "clean" verdict.
// ---------------------------------------------------------------------------

func TestVerifyTeardown(t *testing.T) {
	cases := []struct {
		name        string
		objs        []ctrlclient.Object
		wantErr     bool
		wantOutput  string // substring that must appear in printed output
		notInOutput string // substring that must NOT appear
	}{
		{
			// If this ever returns nil with the wrong output, an operator is
			// told a cluster is gone when it actually is — the core promise
			// of this function.
			name:       "all clear reports success",
			objs:       nil,
			wantErr:    false,
			wantOutput: "✅ Guest cluster",
		},
		{
			// A leftover VM must block the verdict. If this regresses, the
			// tool would tell an operator to strip finalizers while a real
			// VM (and its provider-side resources) still exists.
			name:       "remaining machine blocks the verdict",
			objs:       []ctrlclient.Object{machine(testClusterID + "-node-1")},
			wantErr:    true,
			wantOutput: "machine(s) remain",
		},
		{
			// The KubernetesCluster CR is the resource whose finalizer
			// release is the whole point of phase 2 — if it's still there,
			// nothing has actually been deleted.
			name:       "KubernetesCluster CR still present blocks the verdict",
			objs:       []ctrlclient.Object{testKC()},
			wantErr:    true,
			wantOutput: "KubernetesCluster CR still present",
		},
		{
			// The cpvip has no ownerReference to the cluster (associated by
			// name only), so it is the one resource that would silently leak
			// an external API-server VIP if this check were dropped.
			name:       "cpvip still present blocks the verdict (API VIP not released)",
			objs:       []ctrlclient.Object{cpvip(testClusterID)},
			wantErr:    true,
			wantOutput: "ControlPlaneVirtualSharedIP still present",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl := fakeClient(testScheme(t, false, false), tc.objs...)
			r, out := newTestRunner(t, cl, nil)

			err := r.verifyTeardown(t.Context())

			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil; output:\n%s", out.String())
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil error, got %v; output:\n%s", err, out.String())
			}
			if !strings.Contains(out.String(), tc.wantOutput) {
				t.Fatalf("output missing %q, got:\n%s", tc.wantOutput, out.String())
			}
			if tc.notInOutput != "" && strings.Contains(out.String(), tc.notInOutput) {
				t.Fatalf("output unexpectedly contains %q, got:\n%s", tc.notInOutput, out.String())
			}
			if tc.wantErr {
				// The message is what tells the operator NOT to strip
				// finalizers and instead go read the operator logs. If this
				// wording regresses, a future operator could reasonably
				// conclude the error just means "retry the same command".
				if !strings.Contains(err.Error(), "do NOT strip finalizers") {
					t.Errorf("error must warn against stripping finalizers, got: %v", err)
				}
				if !strings.Contains(err.Error(), "operator logs") {
					t.Errorf("error must point at the operator logs, got: %v", err)
				}
			}
		})
	}
}

// TestVerifyTeardownQueryErrorIsNotClean is the safety property that matters
// most: if the runner cannot even ask whether a machine remains, it must
// treat that as a problem, not as evidence of nothing remaining. Silently
// swallowing a List error here would turn a transient API blip into a false
// "fully deleted" verdict.
func TestVerifyTeardownQueryErrorIsNotClean(t *testing.T) {
	sch := testScheme(t, false, false)
	base := fake.NewClientBuilder().WithScheme(sch).Build()
	cl := fake.NewClientBuilder().WithScheme(sch).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
			if _, ok := list.(*vitiv1alpha1.MachineList); ok {
				return errors.New("etcd unavailable")
			}
			return base.List(ctx, list, opts...)
		},
	}).Build()

	r, out := newTestRunner(t, cl, nil)
	err := r.verifyTeardown(t.Context())

	if err == nil {
		t.Fatal("expected error when the machine query fails, got nil")
	}
	if !strings.Contains(out.String(), "cannot verify machines") {
		t.Fatalf("output should surface the unverifiable state, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "✅") {
		t.Fatalf("must not print a success verdict alongside an unverifiable state, got:\n%s", out.String())
	}
}

// TestVerifyTeardownAlreadyFailedBlocksCleanVerdict covers the mechanism that
// carries forward earlier non-fatal problems (recorded via r.failf during the
// waitUntil calls in teardown) into the final verdict. Without this, a
// timeout on e.g. NetworkConfiguration removal earlier in teardown could be
// silently forgotten by the time verifyTeardown runs its own checks.
func TestVerifyTeardownAlreadyFailedBlocksCleanVerdict(t *testing.T) {
	cl := fakeClient(testScheme(t, false, false)) // nothing remains
	r, out := newTestRunner(t, cl, nil)
	r.failed = true // simulates an earlier non-fatal warning

	err := r.verifyTeardown(t.Context())

	if err == nil {
		t.Fatal("a prior failure must block a clean verdict even when nothing currently remains")
	}
	if strings.Contains(out.String(), "✅") {
		t.Fatalf("must not print the success line when r.failed is true, got:\n%s", out.String())
	}
}

// ---------------------------------------------------------------------------
// countClusterMachines — decides what phase 2 waits for.
// ---------------------------------------------------------------------------

func TestCountClusterMachines(t *testing.T) {
	t.Run("counts only this cluster's machines", func(t *testing.T) {
		// A neighbouring cluster's machine sharing the namespace must never
		// be counted — miscounting here is the "wait forever" or, worse in
		// the delete path, the "declare done too early" hazard.
		otherClusterID := "t-team-002-cd34"
		cl := fakeClient(testScheme(t, false, false),
			machine(testClusterID+"-node-1"),
			machine(testClusterID+"-node-2"),
			machine(otherClusterID+"-node-1"),
		)
		r, _ := newTestRunner(t, cl, nil)

		n, err := r.countClusterMachines(t.Context())

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 2 {
			t.Fatalf("want 2 (only this cluster's machines), got %d", n)
		}
	})

	t.Run("no machines", func(t *testing.T) {
		cl := fakeClient(testScheme(t, false, false))
		r, _ := newTestRunner(t, cl, nil)

		n, err := r.countClusterMachines(t.Context())

		if err != nil || n != 0 {
			t.Fatalf("want (0, nil), got (%d, %v)", n, err)
		}
	})

	t.Run("List error returns negative count and error, never 0-with-nil", func(t *testing.T) {
		// 0-with-nil is indistinguishable from "all machines gone" to every
		// caller (waitUntil, teardown) — a List failure must not be able to
		// masquerade as that.
		sch := testScheme(t, false, false)
		cl := fake.NewClientBuilder().WithScheme(sch).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
				return errors.New("connection refused")
			},
		}).Build()
		r, _ := newTestRunner(t, cl, nil)

		n, err := r.countClusterMachines(t.Context())

		if err == nil {
			t.Fatal("expected an error")
		}
		if n >= 0 {
			t.Fatalf("want a negative count on error, got %d", n)
		}
	})
}

// ---------------------------------------------------------------------------
// remainingIPAllocations — the node-IP leak check.
// ---------------------------------------------------------------------------

func TestRemainingIPAllocations(t *testing.T) {
	t.Run("CRD not installed: not checked, no leak reported", func(t *testing.T) {
		// This is the normal state on management clusters that have not
		// migrated to static IP allocation. The real cluster returns a
		// *meta.NoKindMatchError for the unregistered kind, which the fake
		// client (with the type simply absent from the scheme) does not
		// reproduce on its own — so the classification path is forced here.
		sch := testScheme(t, false, false)
		base := fake.NewClientBuilder().WithScheme(sch).Build()
		cl := fake.NewClientBuilder().WithScheme(sch).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
				if ul, ok := list.(*unstructured.UnstructuredList); ok && ul.GroupVersionKind() == kube.IPAllocationListGVK {
					return &meta.NoKindMatchError{
						GroupKind:        kube.IPAllocationListGVK.GroupKind(),
						SearchedVersions: []string{kube.IPAllocationListGVK.Version},
					}
				}
				return base.List(ctx, list, opts...)
			},
		}).Build()
		r, out := newTestRunner(t, cl, nil)

		leaked, checked := r.remainingIPAllocations(t.Context())

		if checked {
			t.Fatal("checked must be false when the CRD is not installed")
		}
		if leaked != nil {
			t.Fatalf("no leak must be reported when unchecked, got %v", leaked)
		}
		if strings.Contains(out.String(), "could not verify") {
			t.Fatalf("CRD-absent must not be reported as a warning, got:\n%s", out.String())
		}
	})

	t.Run("CRD installed, allocations remain for this cluster: reported as leaks", func(t *testing.T) {
		cl := fakeClient(testScheme(t, true, false),
			ipAllocation(testClusterID+"-node-1"),
			ipAllocation(testClusterID+"-node-2"),
		)
		r, _ := newTestRunner(t, cl, nil)

		leaked, checked := r.remainingIPAllocations(t.Context())

		if !checked {
			t.Fatal("checked must be true when the CRD is installed and the query succeeds")
		}
		if len(leaked) != 2 {
			t.Fatalf("want 2 leaked allocations, got %v", leaked)
		}
	})

	t.Run("CRD installed, allocations belong to a different cluster: not reported", func(t *testing.T) {
		// A neighbouring cluster's IPAllocations must never be reported
		// against this cluster's decommission — that would either block a
		// legitimate clean verdict or, worse, invite someone to go delete
		// another cluster's IP records by hand.
		otherClusterID := "t-team-002-cd34"
		cl := fakeClient(testScheme(t, true, false), ipAllocation(otherClusterID+"-node-1"))
		r, _ := newTestRunner(t, cl, nil)

		leaked, checked := r.remainingIPAllocations(t.Context())

		if !checked {
			t.Fatal("checked must be true")
		}
		if len(leaked) != 0 {
			t.Fatalf("want no leaks reported for another cluster's allocations, got %v", leaked)
		}
	})

	t.Run("generic List error: not checked, warned, never a silent no-leak", func(t *testing.T) {
		sch := testScheme(t, true, false)
		cl := fake.NewClientBuilder().WithScheme(sch).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
				return errors.New("apiserver timeout")
			},
		}).Build()
		r, out := newTestRunner(t, cl, nil)

		leaked, checked := r.remainingIPAllocations(t.Context())

		if checked {
			t.Fatal("checked must be false on a generic query error")
		}
		if leaked != nil {
			t.Fatalf("no leak must be claimed on a generic query error, got %v", leaked)
		}
		if !strings.Contains(out.String(), "could not verify IPAllocations") {
			t.Fatalf("a generic error must produce a visible warning, got:\n%s", out.String())
		}
	})
}

// ---------------------------------------------------------------------------
// teardown — end to end against a fake client.
// ---------------------------------------------------------------------------

// TestTeardownEndToEndNothingPresent covers the happy path only: nothing to
// delete, nothing left over, so teardown must complete and return nil. The
// fake client cannot represent finalizer-driven cascading deletion (a real
// Machine delete would block on its finalizer until the provider-side VM is
// gone; the fake client just removes the object), so this test does not — and
// cannot honestly — cover the "delete triggers real teardown" behaviour. That
// remains validated only by the operations-drift shell scripts and live runs.
func TestTeardownEndToEndNothingPresent(t *testing.T) {
	cl := fakeClient(testScheme(t, false, false)) // no cluster, no machines, no cpvip
	r, out := newTestRunner(t, cl, nil)

	err := r.teardown(t.Context())

	if err != nil {
		t.Fatalf("expected a clean teardown, got: %v; output:\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "✅ Guest cluster") {
		t.Fatalf("expected a success verdict in output, got:\n%s", out.String())
	}
}
