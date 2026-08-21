package netns

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

func nn(ns, name string, vlan int, assoc ...string) *vitiv1alpha1.NetworkNamespace {
	return &vitiv1alpha1.NetworkNamespace{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: vitiv1alpha1.NetworkNamespaceStatus{
			VlanID:                         vlan,
			AssociatedKubernetesClusterIDs: assoc,
		},
	}
}

func kc(ns, name, clusterID, netnsName string) vitiv1alpha1.KubernetesCluster {
	k := vitiv1alpha1.KubernetesCluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	k.Spec.Cluster.ClusterId = clusterID
	k.Spec.Cluster.NetworkNamespaceName = netnsName
	return k
}

func ncByName(ns, name, netnsName string) vitiv1alpha1.NetworkConfiguration {
	c := vitiv1alpha1.NetworkConfiguration{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	c.Spec.NetworkNamespaceName = netnsName
	return c
}

func ncByVlan(ns, name string, vlan int) vitiv1alpha1.NetworkConfiguration {
	c := vitiv1alpha1.NetworkConfiguration{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	c.Spec.NetworkInterfaces = []vitiv1alpha1.NetworkConfigurationInterface{
		{Name: vlanInterfaceName(vlan)},
	}
	return c
}

// ncBare declares neither a netns name nor any interface: nothing to rule it
// in or out with.
func ncBare(ns, name string) vitiv1alpha1.NetworkConfiguration {
	return vitiv1alpha1.NetworkConfiguration{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func ipalloc(ns, name, netnsName string) unstructured.Unstructured {
	u := unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"networkNamespaceName": netnsName},
	}}
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func TestEvidenceKCReferenceBlocks(t *testing.T) {
	target := nn("team-a", "team-a-x1", 2100)
	s := &Snapshot{KCs: []vitiv1alpha1.KubernetesCluster{
		kc("team-a", "c1", "c1-abcd", "team-a-x1"),
		kc("team-a", "c2", "c2-abcd", "team-a-OTHER"), // other netns, same ns
		kc("team-b", "c3", "c3-abcd", "team-a-x1"),    // same name, WRONG namespace
	}}
	ev := EvidenceFor(s, target)
	if len(ev.ReferencingKCs) != 1 || ev.ReferencingKCs[0] != "c1" {
		t.Fatalf("ReferencingKCs = %v, want [c1]", ev.ReferencingKCs)
	}
	if !ev.Blocked() {
		t.Error("a referenced netns must be Blocked")
	}
}

func TestEvidenceNCByNameAndByVlanBlock(t *testing.T) {
	target := nn("team-a", "team-a-x1", 2100)
	s := &Snapshot{NCs: []vitiv1alpha1.NetworkConfiguration{
		ncByName("team-a", "m1-nc", "team-a-x1"),
		ncByVlan("team-a", "m2-nc", 2100),
		ncByVlan("team-a", "m3-nc", 2999),        // different vlan
		ncByName("team-b", "m4-nc", "team-a-x1"), // wrong namespace
	}}
	ev := EvidenceFor(s, target)
	if len(ev.NCRefs) != 2 {
		t.Fatalf("NCRefs = %v, want [m1-nc m2-nc]", ev.NCRefs)
	}
	if !ev.Blocked() {
		t.Error("NC-referenced netns must be Blocked")
	}
}

func TestEvidenceVlanUnknownSkipsVlanMatching(t *testing.T) {
	target := nn("team-a", "team-a-x1", 0) // vlan never assigned
	s := &Snapshot{NCs: []vitiv1alpha1.NetworkConfiguration{
		ncByVlan("team-a", "m1-nc", 0), // pathological "vlan0" iface must NOT match
	}}
	ev := EvidenceFor(s, target)
	if ev.VlanKnown {
		t.Error("VlanKnown must be false for vlanId 0")
	}
	if len(ev.NCRefs) != 0 {
		t.Fatalf("NCRefs = %v, want none when vlan is unknown", ev.NCRefs)
	}
	// Not matching is not the same as being ruled out: with no vlan to compare
	// against and no networkNamespaceName declared, this NC's binding is
	// unknown, so it lands in the fail-closed bucket rather than being
	// silently treated as unrelated.
	if len(ev.NCUnevaluated) != 1 || ev.NCUnevaluated[0] != "m1-nc" {
		t.Errorf("NCUnevaluated = %v, want [m1-nc]", ev.NCUnevaluated)
	}
	if !ev.Blocked() {
		t.Error("an unevaluable NetworkConfiguration must block deletion")
	}
}

func TestEvidenceIPAllocations(t *testing.T) {
	target := nn("team-a", "team-a-x1", 2100)
	s := &Snapshot{IPAllocCRDPresent: true, IPAllocs: []unstructured.Unstructured{
		ipalloc("team-a", "c1-ctp0-vlan2100", "team-a-x1"),
		ipalloc("team-a", "c2-ctp0-vlan2999", "team-a-OTHER"), // other netns
		ipalloc("team-b", "c3-ctp0-vlan2100", "team-a-x1"),    // wrong namespace
	}}
	ev := EvidenceFor(s, target)
	if ev.IPAllocCount != 1 {
		t.Fatalf("IPAllocCount = %d, want 1", ev.IPAllocCount)
	}
	if !ev.Blocked() {
		t.Error("netns with live IPAllocations must be Blocked")
	}
}

// TestEvidenceIPAllocUnreadableFieldFailsClosed pins the third load-bearing
// safety property: an ipallocation whose spec.networkNamespaceName cannot be
// read says NOTHING about which netns it belongs to, and must never be
// counted as "not this one". Before this, the found-bool and error were
// discarded, so a renamed or retyped field made every lookup return "" —
// IPAllocCount 0, gate reported as passed, netns deleted with live
// allocations against it.
func TestEvidenceIPAllocUnreadableFieldFailsClosed(t *testing.T) {
	target := nn("team-a", "team-a-x1", 2100)

	missing := unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	missing.SetNamespace("team-a")
	missing.SetName("no-such-field")

	wrongType := unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"networkNamespaceName": 42}, // not a string
	}}
	wrongType.SetNamespace("team-a")
	wrongType.SetName("wrong-type")

	elsewhere := ipalloc("team-b", "other-namespace", "")

	s := &Snapshot{IPAllocCRDPresent: true, IPAllocs: []unstructured.Unstructured{
		missing, wrongType, elsewhere,
	}}
	ev := EvidenceFor(s, target)

	if ev.IPAllocCount != 0 {
		t.Errorf("IPAllocCount = %d, want 0 — none of these is a confirmed reference", ev.IPAllocCount)
	}
	if len(ev.IPAllocUnevaluated) != 2 {
		t.Fatalf("IPAllocUnevaluated = %v, want the two unreadable records in team-a", ev.IPAllocUnevaluated)
	}
	if !ev.Blocked() {
		t.Error("unreadable ipallocations must block: unknown is not absence")
	}
}

func TestEvidenceIPAllocCRDAbsent(t *testing.T) {
	ev := EvidenceFor(&Snapshot{IPAllocCRDPresent: false}, nn("team-a", "team-a-x1", 2100))
	if ev.IPAllocCount != -1 {
		t.Fatalf("IPAllocCount = %d, want -1 (CRD absent)", ev.IPAllocCount)
	}
	if ev.Blocked() {
		t.Error("CRD absence alone must not block")
	}
}

func TestEvidenceGhostAssociations(t *testing.T) {
	// Stale status lists two ids; only one has a live KC in the namespace.
	target := nn("team-a", "team-a-x1", 2100, "c1-abcd", "ghost-dead")
	s := &Snapshot{KCs: []vitiv1alpha1.KubernetesCluster{
		kc("team-a", "c1", "c1-abcd", "team-a-x1"),
	}}
	ev := EvidenceFor(s, target)
	if len(ev.GhostAssocIDs) != 1 || ev.GhostAssocIDs[0] != "ghost-dead" {
		t.Fatalf("GhostAssocIDs = %v, want [ghost-dead]", ev.GhostAssocIDs)
	}
	// Ghosts are informational: with a live referencing KC this netns is
	// blocked, but the ghost itself must never contribute to Blocked().
}

func TestOrphansSelectsOnlyUnclaimed(t *testing.T) {
	used := nn("team-a", "team-a-x1", 2100)
	orphan := nn("team-b", "team-b-z9", 2200)
	s := &Snapshot{
		NetNSs: []vitiv1alpha1.NetworkNamespace{*used, *orphan},
		KCs:    []vitiv1alpha1.KubernetesCluster{kc("team-a", "c1", "c1-abcd", "team-a-x1")},
	}
	got := Orphans(s)
	if len(got) != 1 || got[0].NN.Name != "team-b-z9" {
		t.Fatalf("Orphans = %d hits, want exactly team-b-z9", len(got))
	}
}

func TestOrphansExcludesNetNSClaimedOnlyByVlanInterface(t *testing.T) {
	// The regression this exists for. d-trd-atlas-001 (ptr1, created
	// 2026-02-17) is live and Ready but names no netns, because the operator
	// did not write spec.data.networkNamespaceName until ~2026-06-02. Its
	// NetworkConfigurations do not carry the name either — only a vlan2470
	// interface. Filtering on the KC reference alone listed a live 6-node
	// cluster's VLAN as an orphan.
	target := nn("vitistack-atlas", "vitistack-atlas", 2470)
	s := &Snapshot{
		NetNSs: []vitiv1alpha1.NetworkNamespace{*target},
		// No KC references it: the pre-June cluster names no netns at all.
		KCs: []vitiv1alpha1.KubernetesCluster{kc("vitistack-atlas", "d-trd-atlas-001", "d-trd-atlas-001-xdf2", "")},
		NCs: []vitiv1alpha1.NetworkConfiguration{
			ncByVlan("vitistack-atlas", "d-trd-atlas-001-xdf2-ctp0", 2470),
			ncByVlan("vitistack-atlas", "d-trd-atlas-001-xdf2-wrk3", 2470),
		},
	}
	if got := Orphans(s); len(got) != 0 {
		t.Fatalf("Orphans = %d hits, want 0: a netns whose vlan is carrying "+
			"machines is in use, whatever the KubernetesCluster fails to say", len(got))
	}
}

func TestOrphansExcludesNetNSClaimedByLiveIPAllocation(t *testing.T) {
	target := nn("team-a", "team-a-x1", 2100)
	s := &Snapshot{
		NetNSs:            []vitiv1alpha1.NetworkNamespace{*target},
		IPAllocs:          []unstructured.Unstructured{ipalloc("team-a", "alloc-1", "team-a-x1")},
		IPAllocCRDPresent: true,
	}
	if got := Orphans(s); len(got) != 0 {
		t.Fatalf("Orphans = %d hits, want 0: a live IPAllocation is a claim", len(got))
	}
}

func TestOrphansStillListsUnreadableIPAllocations(t *testing.T) {
	// Unknown is not a claim, so the netns is listed — but it is not
	// deletable, and dropping it would hide the only genuinely anomalous
	// case (a schema change or a hand-edited record) behind a clean sweep.
	target := nn("team-a", "team-a-x1", 2100)
	broken := unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	broken.SetNamespace("team-a")
	broken.SetName("alloc-broken")
	s := &Snapshot{
		NetNSs:            []vitiv1alpha1.NetworkNamespace{*target},
		IPAllocs:          []unstructured.Unstructured{broken},
		IPAllocCRDPresent: true,
	}
	got := Orphans(s)
	if len(got) != 1 {
		t.Fatalf("Orphans = %d hits, want 1: an unreadable record must not be silently swept", len(got))
	}
	if got[0].Ev.InUse() {
		t.Error("an unreadable ipallocation is not a claim — InUse must stay false")
	}
	if !got[0].Ev.Blocked() {
		t.Error("an unreadable ipallocation must still block deletion (fail-closed)")
	}
}

func TestBlockedAndInUseDivergeOnlyOnUnresolvedEvidence(t *testing.T) {
	// nn delete's two gates and the kc delete advisory all key on Blocked, so
	// the exact relationship between the two predicates is load-bearing:
	// Blocked = InUse || Unresolved, and nothing else moves either one.
	cases := []struct {
		name      string
		ev        Evidence
		wantBlock bool
		wantInUse bool
	}{
		{"nothing", Evidence{IPAllocCount: -1}, false, false},
		{"kc ref", Evidence{IPAllocCount: -1, ReferencingKCs: []string{"c1"}}, true, true},
		{"nc ref", Evidence{IPAllocCount: -1, NCRefs: []string{"nc1"}}, true, true},
		{"live ipalloc", Evidence{IPAllocCount: 1}, true, true},
		{"zero ipallocs", Evidence{IPAllocCount: 0}, false, false},
		// Unresolved: blocks without being a claim.
		{"unreadable ipalloc", Evidence{IPAllocCount: 0, IPAllocUnevaluated: []string{"a1"}}, true, false},
		{"unevaluable nc", Evidence{IPAllocCount: -1, NCUnevaluated: []string{"nc1"}}, true, false},
		// Never gates anything.
		{"ghosts only", Evidence{IPAllocCount: -1, GhostAssocIDs: []string{"dead"}}, false, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			ev := tt.ev
			if got := ev.Blocked(); got != tt.wantBlock {
				t.Errorf("Blocked() = %v, want %v", got, tt.wantBlock)
			}
			if got := ev.InUse(); got != tt.wantInUse {
				t.Errorf("InUse() = %v, want %v", got, tt.wantInUse)
			}
			if got := ev.Blocked(); got != (ev.InUse() || ev.Unresolved()) {
				t.Errorf("Blocked() = %v but InUse||Unresolved = %v — the gates have drifted apart",
					got, ev.InUse() || ev.Unresolved())
			}
		})
	}
}

func TestEvidenceNCUnevaluableWhenVlanUnknown(t *testing.T) {
	// The hole this closes. A netns with no status.vlanId cannot use the
	// vlan<id> rule, and a pre-2026-06-02 NetworkConfiguration declares no
	// networkNamespaceName either — so there is nothing left to rule it out
	// with. Before this, that read as "not a reference", which is how an
	// in-use netns whose status was lost would have become deletable.
	target := nn("vitistack-atlas", "vitistack-atlas", 0) // vlan unknown
	s := &Snapshot{
		NetNSs: []vitiv1alpha1.NetworkNamespace{*target},
		NCs: []vitiv1alpha1.NetworkConfiguration{
			ncByVlan("vitistack-atlas", "d-trd-atlas-001-xdf2-ctp0", 2470),
		},
	}
	ev := EvidenceFor(s, target)
	if ev.VlanKnown {
		t.Fatal("fixture is wrong: vlan must be unknown for this case")
	}
	if len(ev.NCUnevaluated) != 1 || ev.NCUnevaluated[0] != "d-trd-atlas-001-xdf2-ctp0" {
		t.Fatalf("NCUnevaluated = %v, want the one NC whose binding cannot be determined", ev.NCUnevaluated)
	}
	if ev.InUse() {
		t.Error("unknown is not a claim — InUse must stay false")
	}
	if !ev.Blocked() {
		t.Error("unknown is not absence either — deletion must be refused (fail-closed)")
	}
	// Symmetric with unreadable ipallocations: still reported, so the anomaly
	// reaches a human instead of being swept up as clean.
	if got := Orphans(s); len(got) != 1 {
		t.Fatalf("Orphans = %d, want 1: an unresolvable netns must still be listed", len(got))
	}
}

func TestEvidenceNCUnevaluableWithNoInterfaces(t *testing.T) {
	// Same hole, reached without losing the vlan: an NC that declares neither
	// a netns name nor any interface cannot be ruled out either. No such
	// object existed in the fleet when this was written (all 775 NCs had at
	// least one interface), but it is the same class and costs nothing.
	target := nn("team-a", "team-a-x1", 2100)
	s := &Snapshot{
		NetNSs: []vitiv1alpha1.NetworkNamespace{*target},
		NCs:    []vitiv1alpha1.NetworkConfiguration{ncBare("team-a", "nc-bare")},
	}
	ev := EvidenceFor(s, target)
	if len(ev.NCUnevaluated) != 1 {
		t.Fatalf("NCUnevaluated = %v, want [nc-bare]", ev.NCUnevaluated)
	}
	if !ev.Blocked() || ev.InUse() {
		t.Errorf("want blocked-but-not-in-use, got Blocked=%v InUse=%v", ev.Blocked(), ev.InUse())
	}
}

func TestEvidenceNCsThatCanBeRuledOutAreNotUnevaluable(t *testing.T) {
	// The fail-closed gate must not swallow the cases that ARE settled, or
	// every netns in a busy namespace becomes permanently undeletable.
	target := nn("team-a", "team-a-x1", 2100)
	s := &Snapshot{
		NetNSs: []vitiv1alpha1.NetworkNamespace{*target},
		NCs: []vitiv1alpha1.NetworkConfiguration{
			// Names a different netns: settled, not ours.
			ncByName("team-a", "nc-other-netns", "team-a-SOMETHING-ELSE"),
			// Has interfaces, but on another vlan, and our vlan is known:
			// settled, bound elsewhere.
			ncByVlan("team-a", "nc-other-vlan", 2999),
			// Wrong namespace entirely.
			ncBare("team-b", "nc-elsewhere"),
		},
	}
	ev := EvidenceFor(s, target)
	if len(ev.NCUnevaluated) != 0 {
		t.Fatalf("NCUnevaluated = %v, want none: every one of these can be ruled out", ev.NCUnevaluated)
	}
	if len(ev.NCRefs) != 0 {
		t.Fatalf("NCRefs = %v, want none", ev.NCRefs)
	}
	if ev.Blocked() {
		t.Error("nothing here claims or obscures the netns — it must be deletable")
	}
}
