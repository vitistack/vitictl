package netns

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

// newScheme registers the typed vitistack API; withIPAlloc additionally
// registers the unstructured v1alpha2 ipallocations kind, simulating a zone
// where the static-ip-operator CRD is installed.
func newScheme(t *testing.T, withIPAlloc bool) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := vitiv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if withIPAlloc {
		gv := schema.GroupVersion{Group: "vitistack.io", Version: "v1alpha2"}
		sch.AddKnownTypeWithName(gv.WithKind("IPAllocation"), &unstructured.Unstructured{})
		sch.AddKnownTypeWithName(gv.WithKind("IPAllocationList"), &unstructured.UnstructuredList{})
		metav1.AddToGroupVersion(sch, gv)
	}
	return sch
}

func TestLoadCollectsAllTypes(t *testing.T) {
	sch := newScheme(t, true)
	netnsObj := nn("team-a", "team-a-x1", 2100)
	kcObj := kc("team-a", "c1", "c1-abcd", "team-a-x1")
	ncObj := ncByName("team-a", "m1-nc", "team-a-x1")
	ia := ipalloc("team-a", "c1-ctp0-vlan2100", "team-a-x1")
	ia.SetGroupVersionKind(schema.GroupVersionKind{Group: "vitistack.io", Version: "v1alpha2", Kind: "IPAllocation"})

	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(netnsObj, &kcObj, &ncObj, &ia).Build()

	s, err := Load(t.Context(), c, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.NetNSs) != 1 || len(s.KCs) != 1 || len(s.NCs) != 1 {
		t.Fatalf("counts netns/kc/nc = %d/%d/%d, want 1/1/1", len(s.NetNSs), len(s.KCs), len(s.NCs))
	}
	if !s.IPAllocCRDPresent || len(s.IPAllocs) != 1 {
		t.Fatalf("ipallocs present=%v n=%d, want true/1", s.IPAllocCRDPresent, len(s.IPAllocs))
	}
}

func TestLoadNamespaceScoped(t *testing.T) {
	sch := newScheme(t, true)
	a := nn("team-a", "team-a-x1", 2100)
	b := nn("team-b", "team-b-z9", 2200)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(a, b).Build()

	s, err := Load(t.Context(), c, "team-b")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.NetNSs) != 1 || s.NetNSs[0].Name != "team-b-z9" {
		t.Fatalf("NetNSs = %v, want only team-b-z9", len(s.NetNSs))
	}
}

func TestLoadIPAllocCRDAbsent(t *testing.T) {
	// The brief's original design was a scheme WITHOUT the ipallocations kind,
	// expecting the fake client to fail List with a no-kind-registered error.
	// Empirically (controller-runtime v0.24.1) that does not happen: fake
	// client's addToSchemeIfUnknownAndUnstructuredOrPartial auto-registers any
	// *unstructured.UnstructuredList carrying an explicit GVK on first List(),
	// so the call instead succeeds with err == nil and zero items — which is
	// indistinguishable from "CRD present but empty" and can't exercise Load's
	// CRD-absent branch at all. See task-2-report.md for the full trace.
	//
	// A real cluster's genuine failure mode for an uninstalled CRD is the
	// RESTMapper finding no match: meta.NoKindMatchError. We inject that via
	// an interceptor so this test exercises the real ptr1/bgo condition
	// precisely, without loosening Load's error-matching switch in any way.
	sch := newScheme(t, false)
	noMatch := &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: "vitistack.io", Kind: "IPAllocation"},
		SearchedVersions: []string{"v1alpha2"},
	}
	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(nn("team-a", "team-a-x1", 2100)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
				if list.GetObjectKind().GroupVersionKind() == ipAllocationListGVK {
					return noMatch
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()

	s, err := Load(t.Context(), c, "")
	if err != nil {
		t.Fatalf("Load must tolerate an absent ipallocations CRD, got: %v", err)
	}
	if s.IPAllocCRDPresent {
		t.Error("IPAllocCRDPresent must be false when the kind does not exist")
	}
}

// TestLoadIPAllocGenericListErrorPropagates covers the safety-critical
// default: branch of Load's ipallocations switch — an opaque error (etcd
// timeout, RBAC denial, whatever) is NOT one of the two recognized
// CRD-absence signals and must be returned as a real error, never silently
// read as "CRD absent" (which would wrongly report IPAllocCount == -1 and
// let a blocked deletion through as clear).
func TestLoadIPAllocGenericListErrorPropagates(t *testing.T) {
	sch := newScheme(t, true)
	genericErr := errors.New("etcd timeout")
	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(nn("team-a", "team-a-x1", 2100)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
				if list.GetObjectKind().GroupVersionKind() == ipAllocationListGVK {
					return genericErr
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()

	s, err := Load(t.Context(), c, "")
	if err == nil {
		t.Fatal("Load must return an error for a generic ipallocations List failure, got nil")
	}
	if !errors.Is(err, genericErr) {
		t.Errorf("Load error = %v, want it to wrap %v", err, genericErr)
	}
	if s != nil {
		t.Errorf("Load must return a nil Snapshot on error, got %+v", s)
	}
}

// TestLoadKubernetesClusterListErrorPropagates covers the same
// error-must-not-be-swallowed requirement for one of the typed List calls
// (not just the unstructured ipallocations path): a generic failure listing
// KubernetesClusters must abort Load with an error, not return a partially
// populated Snapshot.
func TestLoadKubernetesClusterListErrorPropagates(t *testing.T) {
	sch := newScheme(t, true)
	genericErr := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(nn("team-a", "team-a-x1", 2100)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
				if _, ok := list.(*vitiv1alpha1.KubernetesClusterList); ok {
					return genericErr
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()

	s, err := Load(t.Context(), c, "")
	if err == nil {
		t.Fatal("Load must return an error for a generic KubernetesClusterList failure, got nil")
	}
	if !errors.Is(err, genericErr) {
		t.Errorf("Load error = %v, want it to wrap %v", err, genericErr)
	}
	if s != nil {
		t.Errorf("Load must return a nil Snapshot on error, got %+v", s)
	}
}
