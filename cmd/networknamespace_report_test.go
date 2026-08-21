package cmd

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/netns"
	"github.com/vitistack/vitictl/internal/printer"
)

func orphanFixture(ev netns.Evidence) netns.Orphan {
	return netns.Orphan{
		NN: &vitiv1alpha1.NetworkNamespace{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "team-a-x1"},
			Status: vitiv1alpha1.NetworkNamespaceStatus{
				Phase: "Ready", VlanID: 2100, IPv4Prefix: "100.64.1.0/24",
			},
		},
		Ev: ev,
	}
}

func TestOrphanReportMarksDeletableFromTheSameGates(t *testing.T) {
	clean := newOrphanReport("az1", orphanFixture(netns.Evidence{IPAllocCount: 0}))
	if !clean.Deletable {
		t.Error("an unblocked orphan must report deletable=true")
	}
	// The only way a LISTED orphan is undeletable: netns.Orphans excludes
	// anything a KubernetesCluster, NetworkConfiguration or live IPAllocation
	// claims, so an unreadable ipallocation record is what remains.
	blocked := newOrphanReport("az1", orphanFixture(netns.Evidence{
		IPAllocCount: 0, IPAllocUnevaluated: []string{"alloc-broken"},
	}))
	if blocked.Deletable {
		t.Error("an orphan with an unreadable IPAllocation must report deletable=false")
	}
}

func TestOrphanReportPreservesCRDAbsentSentinel(t *testing.T) {
	// -1 (CRD not installed) must stay distinguishable from 0 (installed, none
	// found) in structured output, or a consumer cannot tell "gate did not run"
	// from "gate passed".
	r := newOrphanReport("az1", orphanFixture(netns.Evidence{IPAllocCount: -1}))
	if r.IPAllocCount != -1 {
		t.Fatalf("IPAllocCount = %d, want -1 preserved", r.IPAllocCount)
	}
	if r.ipAllocCell != "n/a" {
		t.Errorf("table cell = %q, want n/a", r.ipAllocCell)
	}
}

func TestIPAllocCellFlagsUnevaluatedRecords(t *testing.T) {
	cell := ipAllocCell(netns.Evidence{IPAllocCount: 2, IPAllocUnevaluated: []string{"a", "b"}})
	if cell != "2+2?" {
		t.Errorf("cell = %q, want 2+2? so unreadable records are visible in the table", cell)
	}
}

func TestWriteOrphanAuditJSONCarriesCoverage(t *testing.T) {
	var b strings.Builder
	err := writeOrphanAudit(&b, printer.FormatJSON, orphanAudit{
		Orphans:         []orphanReport{},
		ZonesAudited:    2,
		ZonesConfigured: 3,
	})
	if err != nil {
		t.Fatalf("writeOrphanAudit: %v", err)
	}
	out := b.String()
	// Coverage must survive the pipe: downstream of a redirect the stderr
	// warnings and the exit code are both gone.
	for _, want := range []string{`"zonesAudited": 2`, `"zonesConfigured": 3`, `"complete": false`} {
		if !strings.Contains(out, want) {
			t.Errorf("json output must contain %s, got:\n%s", want, out)
		}
	}
}

func TestWriteOrphanAuditMarksCompleteCoverage(t *testing.T) {
	var b strings.Builder
	if err := writeOrphanAudit(&b, printer.FormatYAML, orphanAudit{
		Orphans: []orphanReport{}, ZonesAudited: 3, ZonesConfigured: 3,
	}); err != nil {
		t.Fatalf("writeOrphanAudit: %v", err)
	}
	if !strings.Contains(b.String(), "complete: true") {
		t.Errorf("yaml output should mark a full audit complete, got:\n%s", b.String())
	}
}
