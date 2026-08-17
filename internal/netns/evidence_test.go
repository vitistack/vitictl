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

func TestOrphansSelectsOnlyUnreferenced(t *testing.T) {
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
	// An orphan (no KC refs) can still carry blocking evidence (NC/ipalloc)
	// — Orphans lists it anyway; delete's gates are what refuse.
}
