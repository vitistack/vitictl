package decommission

import (
	"context"
	"errors"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func validatingWebhook(name string) *admissionv1.ValidatingWebhookConfiguration {
	return &admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func mutatingWebhook(name string) *admissionv1.MutatingWebhookConfiguration {
	return &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func TestDeleteAdmissionWebhooksRemovesBothKinds(t *testing.T) {
	sch := testScheme(t, false, false)
	guest := fakeClient(sch,
		validatingWebhook("kyverno-resource-validating-webhook-cfg"),
		validatingWebhook("kyverno-policy-validating-webhook-cfg"),
		mutatingWebhook("kyverno-policy-mutating-webhook-cfg"),
	)
	r, out := newTestRunner(t, nil, guest)

	sweep := r.deleteAdmissionWebhooks(t.Context())
	if len(sweep.Gone) != 3 || len(sweep.Remaining) != 0 {
		t.Fatalf("sweep = %+v, want 3 gone and none remaining", sweep)
	}

	var vals admissionv1.ValidatingWebhookConfigurationList
	if err := guest.List(t.Context(), &vals); err != nil || len(vals.Items) != 0 {
		t.Errorf("validating configurations remaining = %d (err %v), want 0", len(vals.Items), err)
	}
	var muts admissionv1.MutatingWebhookConfigurationList
	if err := guest.List(t.Context(), &muts); err != nil || len(muts.Items) != 0 {
		t.Errorf("mutating configurations remaining = %d (err %v), want 0", len(muts.Items), err)
	}
	if !strings.Contains(out.String(), "kyverno-resource-validating-webhook-cfg") {
		t.Errorf("output must name each configuration it removed, got:\n%s", out)
	}
}

// TestDeleteAdmissionWebhooksIsQuietWhenThereAreNone guards against the step
// announcing removals on the majority of clusters that have no webhooks.
func TestDeleteAdmissionWebhooksIsQuietWhenThereAreNone(t *testing.T) {
	sch := testScheme(t, false, false)
	r, out := newTestRunner(t, nil, fakeClient(sch))

	if sweep := r.deleteAdmissionWebhooks(t.Context()); len(sweep.Gone) != 0 || len(sweep.Remaining) != 0 {
		t.Fatalf("sweep = %+v, want empty", sweep)
	}
	if out.String() != "" {
		t.Errorf("output = %q, want empty when there is nothing to remove", out)
	}
	if r.failed {
		t.Error("no webhook configurations must not be treated as a failure")
	}
}

// TestDeleteAdmissionWebhooksFailureDoesNotBlockVerdict pins the grading of
// this step. It exists to unblock the writes that follow, not as a goal in
// itself: a configuration left behind in a cluster about to be deleted harms
// nothing, and if it really was blocking, the steps after it fail loudly and
// those failures are what make the verdict NOT CLEAN. Grading it as a
// failure here would make every RBAC-restricted cluster report NOT CLEAN for
// a reason that did not matter.
func TestDeleteAdmissionWebhooksFailureDoesNotBlockVerdict(t *testing.T) {
	for _, tc := range []struct {
		name  string
		funcs interceptor.Funcs
	}{
		{
			name: "list fails",
			funcs: interceptor.Funcs{
				List: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectList, ...ctrlclient.ListOption) error {
					return errBoom
				},
			},
		},
		{
			name: "delete fails",
			funcs: interceptor.Funcs{
				Delete: func(context.Context, ctrlclient.WithWatch, ctrlclient.Object, ...ctrlclient.DeleteOption) error {
					return errBoom
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sch := testScheme(t, false, false)
			base := fakeClient(sch, validatingWebhook("kyverno-resource-validating-webhook-cfg"))
			guest := interceptor.NewClient(base.(ctrlclient.WithWatch), tc.funcs)
			r, out := newTestRunner(t, nil, guest)

			r.deleteAdmissionWebhooks(t.Context())

			if r.failed {
				t.Error("r.failed = true, want false — removing webhooks is an enabler, not a verdict gate")
			}
			if !strings.Contains(out.String(), "WARNING") {
				t.Errorf("a failure must still be reported, got:\n%s", out)
			}
		})
	}
}

// TestDeleteAdmissionWebhooksNamesTheDeadlock covers the case that leaves an
// operator with no way forward: the deletion of the webhook configuration is
// itself rejected by a webhook. Nothing after it can succeed, so the output
// has to say that rather than let the operator read one more warning in a
// cascade of them.
func TestDeleteAdmissionWebhooksNamesTheDeadlock(t *testing.T) {
	sch := testScheme(t, false, false)
	base := fakeClient(sch, validatingWebhook("gatekeeper-validating-webhook-configuration"))
	guest := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
		Delete: func(context.Context, ctrlclient.WithWatch, ctrlclient.Object, ...ctrlclient.DeleteOption) error {
			return errors.New(`Internal error occurred: failed calling webhook "validation.gatekeeper.sh": ` +
				`Post "https://gatekeeper-webhook-service.gatekeeper-system.svc:443/v1/admit?timeout=3s": ` +
				`dial tcp 10.96.0.12:443: connect: connection refused`)
		},
	})
	r, out := newTestRunner(t, nil, guest)

	r.deleteAdmissionWebhooks(t.Context())

	if !strings.Contains(out.String(), "removed by hand") {
		t.Errorf("output must tell the operator this needs manual removal, got:\n%s", out)
	}
}

func TestIsWebhookError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{
			name: "unreachable webhook",
			err:  errors.New(`Internal error occurred: failed calling webhook "validate.kyverno.svc-fail": connection refused`),
			want: true,
		},
		{
			name: "policy denial is not a webhook failure",
			err:  errors.New(`admission webhook "validate.kyverno.svc-fail" denied the request: requires a team label`),
			want: false,
		},
		{name: "unrelated", err: errBoom, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWebhookError(tc.err); got != tc.want {
				t.Errorf("isWebhookError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestPrecleanRemovesWebhooksBeforeAnyOtherWrite pins the ordering, which is
// the whole point of the step. Every write in phase 1 is routed through
// admission, including the very first one — the patch that stops ArgoCD — so
// a webhook removal placed anywhere but first can be blocked by the thing it
// exists to remove.
func TestPrecleanRemovesWebhooksBeforeAnyOtherWrite(t *testing.T) {
	sch := testScheme(t, false, false)
	base := fakeClient(sch,
		validatingWebhook("kyverno-resource-validating-webhook-cfg"),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "argocd"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "argocd", Name: "argocd-application-controller"}},
	)

	var ops []string
	guest := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, o ...ctrlclient.DeleteOption) error {
			switch obj.(type) {
			case *admissionv1.ValidatingWebhookConfiguration, *admissionv1.MutatingWebhookConfiguration:
				ops = append(ops, "delete-webhook")
			default:
				ops = append(ops, "delete-other")
			}
			return c.Delete(ctx, obj, o...)
		},
		Patch: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, p ctrlclient.Patch, o ...ctrlclient.PatchOption) error {
			ops = append(ops, "patch")
			return c.Patch(ctx, obj, p, o...)
		},
	})
	r, _ := newTestRunner(t, nil, guest)
	ctx, cancel := ctxWithCancel(t)
	cancel()

	_ = r.preclean(ctx)

	if len(ops) == 0 {
		t.Fatal("preclean performed no writes at all — the ordering assertion would be vacuous")
	}
	if ops[0] != "delete-webhook" {
		t.Errorf("first write = %q, want the webhook removal; full order: %v", ops[0], ops)
	}
}

// TestDeleteAdmissionWebhooksVerifiesRemovalRatherThanClaimingIt covers a
// configuration held in Terminating by a finalizer. The API accepted the
// delete, but the API server keeps consulting a webhook configuration until
// it actually leaves storage — so it still rejects the writes this step
// exists to unblock, and reporting it as deleted would send the operator
// looking anywhere but at the real cause.
func TestDeleteAdmissionWebhooksVerifiesRemovalRatherThanClaimingIt(t *testing.T) {
	sch := testScheme(t, false, false)
	stuck := validatingWebhook("kyverno-resource-validating-webhook-cfg")
	stuck.Finalizers = []string{"kyverno.io/cleanup"}
	guest := fakeClient(sch, stuck, mutatingWebhook("clean-cfg"))
	r, out := newTestRunner(t, nil, guest)

	sweep := r.deleteAdmissionWebhooks(t.Context())

	if len(sweep.Remaining) != 1 || sweep.Remaining[0] != "validatingwebhookconfiguration/kyverno-resource-validating-webhook-cfg" {
		t.Fatalf("Remaining = %v, want the finalizer-held configuration", sweep.Remaining)
	}
	if len(sweep.Gone) != 1 || sweep.Gone[0] != "mutatingwebhookconfiguration/clean-cfg" {
		t.Fatalf("Gone = %v, want only the one that actually went", sweep.Gone)
	}
	if strings.Contains(out.String(), "Deleted validatingwebhookconfiguration/kyverno-resource") {
		t.Errorf("a configuration still present must not be reported as deleted, got:\n%s", out)
	}
	if !strings.Contains(out.String(), "still present after an accepted delete") {
		t.Errorf("output must report the configuration that did not go, got:\n%s", out)
	}
}

// TestReappearedDistinguishesRecreationFromAStuckFinalizer keeps the second
// pass honest. A configuration the first pass never managed to remove is
// still there for its own reason; calling that an ArgoCD re-creation sends
// the reader to the wrong place entirely.
func TestReappearedDistinguishesRecreationFromAStuckFinalizer(t *testing.T) {
	first := webhookSweep{
		Gone:      []string{"validatingwebhookconfiguration/went"},
		Remaining: []string{"validatingwebhookconfiguration/stuck"},
	}
	tests := []struct {
		name  string
		later webhookSweep
		want  int
	}{
		{
			name:  "the stuck one is not a re-creation",
			later: webhookSweep{Remaining: []string{"validatingwebhookconfiguration/stuck"}},
			want:  0,
		},
		{
			name:  "one that had gone and is back was re-created",
			later: webhookSweep{Gone: []string{"validatingwebhookconfiguration/went"}},
			want:  1,
		},
		{
			name: "mixed",
			later: webhookSweep{
				Gone:      []string{"validatingwebhookconfiguration/went"},
				Remaining: []string{"validatingwebhookconfiguration/stuck", "mutatingwebhookconfiguration/new"},
			},
			want: 2,
		},
		{name: "nothing found", later: webhookSweep{}, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reappeared(tc.later, first); got != tc.want {
				t.Errorf("reappeared() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestDeleteAdmissionWebhooksUnverifiableRemovalIsNotClaimedAsGone: when the
// verification read fails there is no evidence either way, and the honest
// answer is the one that does not let a later message claim a re-creation.
func TestDeleteAdmissionWebhooksUnverifiableRemovalIsNotClaimedAsGone(t *testing.T) {
	sch := testScheme(t, false, false)
	base := fakeClient(sch, validatingWebhook("kyverno-resource-validating-webhook-cfg"))
	calls := 0
	guest := interceptor.NewClient(base.(ctrlclient.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
			calls++
			if calls > 2 { // the two reads of the verification pass
				return errBoom
			}
			return c.List(ctx, list, opts...)
		},
	})
	r, out := newTestRunner(t, nil, guest)

	sweep := r.deleteAdmissionWebhooks(t.Context())

	if len(sweep.Gone) != 0 {
		t.Errorf("Gone = %v, want none — removal was never verified", sweep.Gone)
	}
	if len(sweep.Remaining) != 1 {
		t.Errorf("Remaining = %v, want the unverified configuration", sweep.Remaining)
	}
	if !strings.Contains(out.String(), "could not verify") {
		t.Errorf("output must say verification failed, got:\n%s", out)
	}
}
