package decommission

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// --- verifyPreclean ---------------------------------------------------
//
// verifyPreclean is the gate that authorises phase 2 (destroying the guest
// VMs). Every case below documents a way a false "clean" verdict would leak
// external state (IPAM addresses, DNS records, real storage volumes) that
// nothing will ever clean up again once the VMs are gone.

// TestVerifyPrecleanCleanGuest: a genuinely empty guest must produce a nil
// error and the success line. If this regresses to always failing, every
// decommission would wrongly refuse to proceed even when there is nothing
// left to clean up.
func TestVerifyPrecleanCleanGuest(t *testing.T) {
	sch := testScheme(t, false, true)
	guest := fakeClient(sch)
	r, out := newTestRunner(t, nil, guest)

	if err := r.verifyPreclean(t.Context()); err != nil {
		t.Fatalf("verifyPreclean() = %v, want nil for an empty guest", err)
	}
	if !strings.Contains(out.String(), "Preclean verified clean") {
		t.Errorf("output = %q, want the success line", out.String())
	}
}

// TestVerifyPrecleanBlocksOnRemainingResources is the core false-clean guard:
// each of these resource kinds represents external state that is not yet
// released, so any one of them present must produce a NOT CLEAN verdict.
// A regression here means phase 2 could destroy VMs while IPAM addresses,
// DNS records, or real storage volumes are still allocated externally.
func TestVerifyPrecleanBlocksOnRemainingResources(t *testing.T) {
	tests := []struct {
		name string
		objs []ctrlclient.Object
		want string // substring expected in the printed problem list
	}{
		{
			name: "remaining PVC blocks — its backing volume may not be released",
			objs: []ctrlclient.Object{pvc(testNS, "data-0")},
			want: "remaining PVC: " + testNS + "/data-0",
		},
		{
			name: "remaining PV blocks — a surviving PV means the external volume may still exist in storage",
			objs: []ctrlclient.Object{pv("pv-0001")},
			want: "remaining PV (external volume possibly not deleted): pv-0001",
		},
		{
			name: "remaining VolumeAttachment blocks — CSI has not finished detaching",
			objs: []ctrlclient.Object{volumeAttachment("va-0001")},
			want: "remaining VolumeAttachment: va-0001",
		},
		{
			name: "remaining LoadBalancer Service blocks — its IPAM address is still allocated",
			objs: []ctrlclient.Object{lbService(testNS, "web")},
			want: "remaining LoadBalancer service: " + testNS + "/web",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sch := testScheme(t, false, true)
			guest := fakeClient(sch, tt.objs...)
			r, out := newTestRunner(t, nil, guest)

			err := r.verifyPreclean(t.Context())
			if err == nil {
				t.Fatalf("verifyPreclean() = nil, want NOT CLEAN error for %s", tt.name)
			}
			if !strings.Contains(err.Error(), "NOT CLEAN") {
				t.Errorf("err = %q, want it to contain NOT CLEAN", err.Error())
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("output = %q, want it to contain %q", out.String(), tt.want)
			}
		})
	}
}

// TestVerifyPrecleanClusterIPDoesNotBlock: a ClusterIP service holds no
// external state (no IPAM allocation, no cloud LB), so its presence must
// never block the verdict. If this regressed to blocking on any Service,
// operators would be stuck unable to ever decommission a cluster with
// internal services still running.
func TestVerifyPrecleanClusterIPDoesNotBlock(t *testing.T) {
	sch := testScheme(t, false, true)
	guest := fakeClient(sch, clusterIPService(testNS, "internal-api"))
	r, _ := newTestRunner(t, nil, guest)

	if err := r.verifyPreclean(t.Context()); err != nil {
		t.Fatalf("verifyPreclean() = %v, want nil — ClusterIP services hold no external state", err)
	}
}

// TestVerifyPrecleanGatewayCRDMissingIsTolerated: not every guest cluster has
// Gateway API installed. That absence (a NoKindMatchError from the RESTMapper)
// must be treated as "nothing to report", not as a blocking problem or a
// verification failure — otherwise every guest without Gateway API installed
// would be permanently undecommissionable.
func TestVerifyPrecleanGatewayCRDMissingIsTolerated(t *testing.T) {
	sch := testScheme(t, false, false) // Gateway kind deliberately NOT registered
	base := fakeClient(sch)

	// Force the real classification path (meta.IsNoMatchError) rather than
	// relying on whatever error shape the fake client happens to produce for
	// an unregistered unstructured kind.
	noKindMatch := &meta.NoKindMatchError{
		GroupKind: schema.GroupKind{Group: gatewayListGVK.Group, Kind: "Gateway"},
	}
	guest := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind() == gatewayListGVK {
				return noKindMatch
			}
			return c.List(ctx, list, opts...)
		},
	})
	r, out := newTestRunner(t, nil, guest)

	if err := r.verifyPreclean(t.Context()); err != nil {
		t.Fatalf("verifyPreclean() = %v, want nil — missing Gateway CRD must be tolerated, not reported", err)
	}
	if strings.Contains(out.String(), "Gateway") {
		t.Errorf("output = %q, must not mention Gateway as a problem when the CRD is simply absent", out.String())
	}
}

// TestVerifyPrecleanRemainingGatewayBlocks: when the Gateway CRD IS installed
// and a Gateway object survived deletion, that must block the verdict — the
// controller behind it may still own an external LoadBalancer/IPAM/DNS
// record.
func TestVerifyPrecleanRemainingGatewayBlocks(t *testing.T) {
	sch := testScheme(t, false, true)
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"})
	gw.SetNamespace(testNS)
	gw.SetName("public")
	guest := fakeClient(sch, gw)
	r, out := newTestRunner(t, nil, guest)

	err := r.verifyPreclean(t.Context())
	if err == nil {
		t.Fatal("verifyPreclean() = nil, want NOT CLEAN error for a surviving Gateway")
	}
	if !strings.Contains(out.String(), "remaining Gateway: "+testNS+"/public") {
		t.Errorf("output = %q, want it to name the remaining gateway", out.String())
	}
}

// TestVerifyPrecleanListErrorIsNotClean: a List failure means the verdict
// cannot verify the resource is gone — it must be reported as a problem
// ("cannot verify ..."), never silently treated as "clean". Declaring clean
// on an API error would authorise VM destruction on pure guesswork.
func TestVerifyPrecleanListErrorIsNotClean(t *testing.T) {
	sch := testScheme(t, false, true)
	base := fakeClient(sch)
	guest := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
			if _, ok := list.(*corev1.PersistentVolumeClaimList); ok {
				return errBoom
			}
			return c.List(ctx, list, opts...)
		},
	})
	r, out := newTestRunner(t, nil, guest)

	err := r.verifyPreclean(t.Context())
	if err == nil {
		t.Fatal("verifyPreclean() = nil, want NOT CLEAN — a List error must not be treated as a clean verdict")
	}
	if !strings.Contains(out.String(), "cannot verify PVCs") {
		t.Errorf("output = %q, want it to report the PVC list as unverifiable", out.String())
	}
}

// TestVerifyPrecleanFailedFlagBlocks: r.failed accumulates non-fatal problems
// from earlier preclean steps (e.g. a failed delete). Even when every List
// in verifyPreclean itself comes back empty, a prior failure must still
// block the verdict — otherwise an error swallowed earlier in the pipeline
// would be forgotten by the time the verdict is rendered.
func TestVerifyPrecleanFailedFlagBlocks(t *testing.T) {
	sch := testScheme(t, false, true)
	guest := fakeClient(sch) // empty guest
	r, _ := newTestRunner(t, nil, guest)
	r.failed = true

	if err := r.verifyPreclean(t.Context()); err == nil {
		t.Fatal("verifyPreclean() = nil, want NOT CLEAN — r.failed must block the verdict regardless of what List shows")
	}
}

// TestVerifyPrecleanErrorWording asserts the NOT CLEAN error tells the
// operator not to delete the cluster yet. This is the actionable part of the
// message — an operator misreading a vague error as "safe to proceed" is
// exactly the failure mode this package exists to prevent.
func TestVerifyPrecleanErrorWording(t *testing.T) {
	sch := testScheme(t, false, true)
	guest := fakeClient(sch, pvc(testNS, "stuck"))
	r, _ := newTestRunner(t, nil, guest)

	err := r.verifyPreclean(t.Context())
	if err == nil {
		t.Fatal("expected NOT CLEAN error")
	}
	got := err.Error()
	if !strings.Contains(got, "NOT CLEAN") || !strings.Contains(got, "do not delete the cluster yet") {
		t.Errorf("err = %q, want it to explicitly warn against deleting the cluster", got)
	}
}

// errBoom is a stand-in transient API error used to simulate List failures.
var errBoom = &listBoomError{}

type listBoomError struct{}

func (*listBoomError) Error() string { return "simulated apiserver error" }

// --- podMountsPVC -------------------------------------------------------
//
// podMountsPVC decides which pods must be scaled down before PVC deletion.
// A false negative here leaves a workload holding a volume open while the
// PVC underneath it is deleted.

func TestPodMountsPVC(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "pod with a PVC volume must be scaled down before PVC deletion",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-0"},
				}},
			}}},
			want: true,
		},
		{
			name: "pod with only non-PVC volumes must not be flagged for scale-down",
			pod: &corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{
				{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
				{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "sec", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{}}},
			}}},
			want: false,
		},
		{
			name: "pod with no volumes at all must not be flagged",
			pod:  &corev1.Pod{},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := podMountsPVC(tt.pod); got != tt.want {
				t.Errorf("podMountsPVC() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- deletePVCsAndWaitVolumes --------------------------------------------
//
// This is the volume gate: PVC deletion returning does NOT mean the backing
// volume is gone from external storage. Declaring success before PVs clear
// leaks real storage. The waits use their own hardcoded timeouts (up to
// 120s), so the "PVs never clear" case below is asserted via a short-timeout
// runner-context cancellation rather than waiting out the real timeout, to
// keep this file fast.

// TestDeletePVCsDeletesExistingPVCs: PVCs present in the guest must actually
// be deleted. If delete stops being called, deletePVCsAndWaitVolumes would
// report progress while leaving the claims (and thus the bound volumes)
// behind untouched.
func TestDeletePVCsDeletesExistingPVCs(t *testing.T) {
	sch := testScheme(t, false, true)
	guest := fakeClient(sch, pvc(testNS, "data-0"), pvc(testNS, "data-1"))
	r, _ := newTestRunner(t, nil, guest)

	ctx, cancel := ctxWithCancel(t)
	cancel() // cancel immediately: bounds the PV/VolumeAttachment waits to one failed poll
	r.deletePVCsAndWaitVolumes(ctx)

	var remaining corev1.PersistentVolumeClaimList
	if err := guest.List(context.Background(), &remaining); err != nil {
		t.Fatalf("List PVCs: %v", err)
	}
	if len(remaining.Items) != 0 {
		t.Errorf("PVCs remaining after deletePVCsAndWaitVolumes: %d, want 0 — the delete calls were not issued", len(remaining.Items))
	}
}

// TestDeletePVCsWaitsDoNotReportDoneWhenPVsRemain: with a cancelled context
// the waits exit immediately without ever seeing the resource clear, so they
// must warn ("did not complete"), never print the "done after" success line.
// This is the crux of the doc-comment danger: a refactor that made this
// print "done" unconditionally would let phase 2 proceed while a PV (a real
// external volume) is still sitting there.
func TestDeletePVCsWaitsDoNotReportDoneWhenPVsRemain(t *testing.T) {
	sch := testScheme(t, false, true)
	guest := fakeClient(sch, pv("pv-still-there"))
	r, out := newTestRunner(t, nil, guest)

	ctx, cancel := ctxWithCancel(t)
	cancel() // ctx.Err() != nil short-circuits waitUntil's deadline check on the first poll
	r.deletePVCsAndWaitVolumes(ctx)

	got := out.String()
	if strings.Contains(got, "PVs cleared (external volumes deleted): done after") {
		t.Fatalf("output = %q, must not claim PVs cleared while a PV survives", got)
	}
	if !strings.Contains(got, "did not complete within") {
		t.Errorf("output = %q, want a timeout warning for the PV wait", got)
	}
}

// TestDeletePVCsListFailureIsRecordedAsFailure: a List error while listing
// PVCs must flip r.failed (via failf) so the eventual verdict cannot be
// clean — silently skipping it would let deletePVCsAndWaitVolumes "succeed"
// while never having attempted to delete anything.
func TestDeletePVCsListFailureIsRecordedAsFailure(t *testing.T) {
	sch := testScheme(t, false, true)
	base := fakeClient(sch)
	guest := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
			if _, ok := list.(*corev1.PersistentVolumeClaimList); ok {
				return errBoom
			}
			return c.List(ctx, list, opts...)
		},
	})
	r, out := newTestRunner(t, nil, guest)

	ctx, cancel := ctxWithCancel(t)
	cancel()
	r.deletePVCsAndWaitVolumes(ctx)

	if !r.failed {
		t.Error("r.failed = false, want true after a PVC List failure")
	}
	if !strings.Contains(out.String(), "could not list PVCs") {
		t.Errorf("output = %q, want it to report the PVC list failure", out.String())
	}
}
