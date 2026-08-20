package cmd

import (
	"strings"
	"testing"

	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/settings"
)

func zones(n int) []settings.AvailabilityZone {
	out := make([]settings.AvailabilityZone, n)
	for i := range out {
		out[i] = settings.AvailabilityZone{Name: string(rune('a' + i))}
	}
	return out
}

func clients(n int) []*kube.Client {
	out := make([]*kube.Client, n)
	for i := range out {
		out[i] = &kube.Client{AZ: settings.AvailabilityZone{Name: string(rune('a' + i))}}
	}
	return out
}

func TestRequireWholeFleetAllowsFullCoverage(t *testing.T) {
	if err := requireWholeFleet(clients(3), zones(3), "cluster", "c1"); err != nil {
		t.Fatalf("3 of 3 zones connected must be allowed, got: %v", err)
	}
}

func TestRequireWholeFleetAllowsSingleScopedZone(t *testing.T) {
	// -z narrows ResolveAvailabilityZones to one zone, so a scoped run is
	// unambiguous by definition and must not be blocked.
	if err := requireWholeFleet(clients(1), zones(1), "networknamespace", "nn1"); err != nil {
		t.Fatalf("a scoped single-zone run must be allowed, got: %v", err)
	}
}

func TestRequireWholeFleetRefusesPartialCoverage(t *testing.T) {
	err := requireWholeFleet(clients(2), zones(3), "cluster", "t-prod-001")
	if err == nil {
		t.Fatal("2 of 3 zones connected must be refused for a destructive command")
	}
	msg := err.Error()
	for _, want := range []string{"refusing", "t-prod-001", "1 of 3", "-z"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q so the operator knows how to proceed, got: %s", want, msg)
		}
	}
}

func TestRequireWholeFleetNamesTheResourceKind(t *testing.T) {
	// The same guard serves clusters and networknamespaces; the message must
	// say which, or it reads as the wrong warning entirely.
	err := requireWholeFleet(clients(1), zones(2), "networknamespace", "vitistack-x")
	if err == nil {
		t.Fatal("expected refusal")
	}
	if !strings.Contains(err.Error(), "networknamespace") {
		t.Errorf("error must name the resource kind, got: %s", err)
	}
}
