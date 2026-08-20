package decommission

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
)

// Shared fixtures for the decommission tests. Defined once here because the
// package's tests are split across files by subject; do not redeclare these.

const (
	testNS        = "vitistack-team"
	testClusterID = "t-team-001-ab12"
	testCluster   = "t-team-001"
)

// testScheme registers everything the runner touches. withIPAlloc and
// withGateway control whether those unstructured kinds are known, which is how
// tests simulate a zone where the CRD is not installed.
func testScheme(t *testing.T, withIPAlloc, withGateway bool) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, storagev1.AddToScheme, vitiv1alpha1.AddToScheme,
	} {
		if err := add(sch); err != nil {
			t.Fatal(err)
		}
	}
	if withIPAlloc {
		registerUnstructured(sch, kube.IPAllocationListGVK)
	}
	if withGateway {
		registerUnstructured(sch, gatewayListGVK)
	}
	return sch
}

func registerUnstructured(sch *runtime.Scheme, listGVK schema.GroupVersionKind) {
	gv := listGVK.GroupVersion()
	itemKind := strings.TrimSuffix(listGVK.Kind, "List")
	sch.AddKnownTypeWithName(gv.WithKind(itemKind), &unstructured.Unstructured{})
	sch.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	metav1.AddToGroupVersion(sch, gv)
}

// testKC builds the KubernetesCluster the runner is constructed from.
func testKC() *vitiv1alpha1.KubernetesCluster {
	kc := &vitiv1alpha1.KubernetesCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testCluster},
	}
	kc.Spec.Cluster.ClusterId = testClusterID
	return kc
}

// newTestRunner builds a Runner wired to the given clients, with output
// captured. Either client may be nil when the test does not exercise it.
func newTestRunner(t *testing.T, mgmt, guest ctrlclient.Client) (*Runner, *strings.Builder) {
	t.Helper()
	var out strings.Builder
	r := &Runner{
		opts:      Options{Out: &out, MachineTimeout: 50 * time.Millisecond},
		mgmt:      mgmt,
		guest:     guest,
		cluster:   testKC(),
		clusterID: testClusterID,
		namespace: testNS,
	}
	return r, &out
}

// fakeClient builds a client from objects, with the given scheme.
func fakeClient(sch *runtime.Scheme, objs ...ctrlclient.Object) ctrlclient.Client {
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
}

// machine returns a Machine in the test namespace.
func machine(name string) *vitiv1alpha1.Machine {
	return &vitiv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name}}
}

// cpvip returns a ControlPlaneVirtualSharedIP named exactly for the cluster —
// it has no ownerReference, which is why the runner verifies it by name.
func cpvip(name string) *vitiv1alpha1.ControlPlaneVirtualSharedIP {
	return &vitiv1alpha1.ControlPlaneVirtualSharedIP{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name}}
}

// pvc / pv / volumeAttachment / lbService build the guest-side objects whose
// survival must block a clean verdict.
func pvc(ns, name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func pv(name string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func volumeAttachment(name string) *storagev1.VolumeAttachment {
	return &storagev1.VolumeAttachment{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func lbService(ns, name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}
}

func clusterIPService(ns, name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
	}
}

// ctxWithCancel is a convenience for cancellation tests.
func ctxWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithCancel(t.Context())
}
