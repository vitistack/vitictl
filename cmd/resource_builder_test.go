package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/settings"
)

func machineBinding() resourceBinding[*vitiv1alpha1.Machine, *vitiv1alpha1.MachineList] {
	return resourceBinding[*vitiv1alpha1.Machine, *vitiv1alpha1.MachineList]{
		Use:        "machine",
		Namespaced: true,
		NewList:    func() *vitiv1alpha1.MachineList { return &vitiv1alpha1.MachineList{} },
		Items:      func(l *vitiv1alpha1.MachineList) []*vitiv1alpha1.Machine { return itemsOf(l.Items) },
	}
}

func machineScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := vitiv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return sch
}

func azListClient(name string, c ctrlclient.Client) *kube.Client {
	return &kube.Client{AZ: settings.AvailabilityZone{Name: name}, Ctrl: c}
}

func testMachine(ns, name string) *vitiv1alpha1.Machine {
	return &vitiv1alpha1.Machine{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func TestCollectResourceGathersFromEveryZone(t *testing.T) {
	sch := machineScheme(t)
	a := azListClient("az-a", fake.NewClientBuilder().WithScheme(sch).WithObjects(testMachine("ns", "m1")).Build())
	b := azListClient("az-b", fake.NewClientBuilder().WithScheme(sch).WithObjects(testMachine("ns", "m2")).Build())

	hits, partial := collectResource(t.Context(), []*kube.Client{a, b}, "", machineBinding())
	if partial {
		t.Error("two healthy zones must not report partial coverage")
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	// Order must follow client order, or table output and any downstream
	// diffing become nondeterministic across runs.
	if hits[0].azName != "az-a" || hits[1].azName != "az-b" {
		t.Errorf("results out of client order: %s, %s", hits[0].azName, hits[1].azName)
	}
}

func TestCollectResourceReportsPartialOnZoneFailure(t *testing.T) {
	// A zone that cannot be listed must mark the result partial. Without this
	// the caller reports "no X found on any availability zone" about a fleet it
	// only partly searched — the object may be sitting on the zone that failed.
	sch := machineScheme(t)
	healthy := azListClient("az-a", fake.NewClientBuilder().WithScheme(sch).WithObjects(testMachine("ns", "m1")).Build())
	broken := azListClient("az-b", fake.NewClientBuilder().WithScheme(sch).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectList, ...ctrlclient.ListOption) error {
				return errors.New("apiserver unavailable")
			},
		}).Build())

	hits, partial := collectResource(t.Context(), []*kube.Client{healthy, broken}, "", machineBinding())
	if !partial {
		t.Fatal("a zone that failed to list must set partial")
	}
	// The healthy zone's results still come back: a broken zone degrades the
	// answer, it does not discard it.
	if len(hits) != 1 || hits[0].azName != "az-a" {
		t.Errorf("healthy zone's results should survive, got %d hits", len(hits))
	}
}

func TestCollectResourceBoundsAHangingZone(t *testing.T) {
	// The bug this guards: an unhealthy zone (e.g. one whose CRD conversion
	// webhook cannot be verified) leaves the apiserver waiting, client-go has
	// no deadline of its own, and the command hangs forever with no output.
	saved := listZoneTimeout
	listZoneTimeout = 50 * time.Millisecond
	defer func() { listZoneTimeout = saved }()

	sch := machineScheme(t)
	hanging := azListClient("az-slow", fake.NewClientBuilder().WithScheme(sch).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, _ ctrlclient.WithWatch, _ ctrlclient.ObjectList, _ ...ctrlclient.ListOption) error {
				<-ctx.Done() // block until the per-zone deadline fires
				return ctx.Err()
			},
		}).Build())
	healthy := azListClient("az-ok", fake.NewClientBuilder().WithScheme(sch).WithObjects(testMachine("ns", "m1")).Build())

	done := make(chan struct{})
	var hits []azItem[*vitiv1alpha1.Machine]
	var partial bool
	go func() {
		hits, partial = collectResource(t.Context(), []*kube.Client{hanging, healthy}, "", machineBinding())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("collectResource did not return — the per-zone timeout is not bounding the call")
	}
	if !partial {
		t.Error("a timed-out zone must set partial")
	}
	if len(hits) != 1 || hits[0].azName != "az-ok" {
		t.Errorf("the healthy zone's results should still be returned, got %d hits", len(hits))
	}
}

func TestCollectResourceRunsZonesConcurrently(t *testing.T) {
	// Sequential listing makes the worst case zones × timeout: three unhealthy
	// zones would take 90s at the default. Fanning out keeps it at one timeout.
	saved := listZoneTimeout
	listZoneTimeout = 300 * time.Millisecond
	defer func() { listZoneTimeout = saved }()

	sch := machineScheme(t)
	slow := func(name string) *kube.Client {
		return azListClient(name, fake.NewClientBuilder().WithScheme(sch).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, _ ctrlclient.WithWatch, _ ctrlclient.ObjectList, _ ...ctrlclient.ListOption) error {
					<-ctx.Done()
					return ctx.Err()
				},
			}).Build())
	}

	start := time.Now()
	_, partial := collectResource(t.Context(), []*kube.Client{slow("a"), slow("b"), slow("c")}, "", machineBinding())
	elapsed := time.Since(start)

	if !partial {
		t.Error("all three zones failed; partial must be set")
	}
	// Three sequential 300ms timeouts would be ~900ms; concurrent is ~300ms.
	if elapsed > 700*time.Millisecond {
		t.Errorf("three zones took %s — that looks sequential, not concurrent", elapsed)
	}
}

func TestIncompleteSuffixOnlyWhenPartial(t *testing.T) {
	if incompleteSuffix(false) != "" {
		t.Error("a complete search must not be qualified")
	}
	if !strings.Contains(incompleteSuffix(true), "incomplete") {
		t.Error("a partial search must say so, or an empty result reads as authoritative")
	}
}
