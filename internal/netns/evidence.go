// Package netns implements orphan auditing and gated deletion of
// NetworkNamespace resources. A NetworkNamespace holds external NAM state
// (VLAN, IPv4/IPv6 prefixes, egress IP) released only by the operator via
// networknamespace.vitistack.io/finalizer — so deletion is delete-and-wait,
// never finalizer removal.
//
// Both verbs (orphans, delete) compute in-use-ness from the same Snapshot +
// EvidenceFor pair, so the two can never disagree about what claims a netns.
// They read that evidence at different thresholds: orphans lists what nothing
// claims (InUse), delete additionally refuses what cannot be resolved
// (Blocked). Everything orphans lists is therefore deletable except where the
// evidence itself is unreadable, which the report says out loud.
package netns

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

// Snapshot is one availability zone's relevant objects, loaded with one List
// per resource type (never per-namespace fan-out).
type Snapshot struct {
	NetNSs []vitiv1alpha1.NetworkNamespace
	KCs    []vitiv1alpha1.KubernetesCluster
	NCs    []vitiv1alpha1.NetworkConfiguration
	// IPAllocs is unstructured: the ipallocations CRD (vitistack.io/v1alpha2)
	// is only rolled out where the static-ip-operator runs.
	IPAllocs          []unstructured.Unstructured
	IPAllocCRDPresent bool
}

// Evidence is everything known about whether one NetworkNamespace is in use.
// The stale status summary (associatedKubernetesClusterIds) is surfaced as
// GhostAssocIDs for display but never gates anything.
type Evidence struct {
	ReferencingKCs []string // KCs in the same namespace with spec.data.networkNamespaceName == name → HARD GATE
	NCRefs         []string // NCs in the same namespace referencing by name or by vlan<id> interface → HARD GATE
	IPAllocCount   int      // ipallocations referencing this netns; -1 = CRD absent on this zone
	// IPAllocUnevaluated names ipallocations in the netns' namespace whose
	// spec.networkNamespaceName could not be read (absent field, or not a
	// string). Their target is UNKNOWN, which is not the same as "not this
	// netns" — so they are a HARD GATE too: an unreadable record must never
	// be counted as evidence of absence.
	IPAllocUnevaluated []string
	GhostAssocIDs      []string // status association ids with no live KC in the namespace — informational
	VlanKnown          bool     // status.vlanId assigned; when false the vlan-interface match cannot apply
}

// InUse reports whether anything positively claims this netns: a
// KubernetesCluster naming it, a NetworkConfiguration bound to it (by name or
// by vlan<id> interface), or a live IPAllocation pointing at it.
//
// Unevaluated ipallocations are deliberately NOT in use. Unknown is not a
// claim, and it is not absence either — which is why it blocks deletion
// (below) while still leaving the netns worth reporting.
func (e *Evidence) InUse() bool {
	return len(e.ReferencingKCs) > 0 || len(e.NCRefs) > 0 || e.IPAllocCount > 0
}

// Blocked reports whether any hard gate refuses deletion. Unevaluated
// ipallocations block on purpose (fail-closed): the alternative is deleting
// external NAM state while records that may point at it are unaccounted for.
//
// This is InUse plus that fail-closed case, which is exactly the set of gates
// it has always been — the split exists so the audit can distinguish "in use"
// from "not deletable", not to change what delete refuses.
func (e *Evidence) Blocked() bool {
	return e.InUse() || len(e.IPAllocUnevaluated) > 0
}

func vlanInterfaceName(vlan int) string { return fmt.Sprintf("vlan%d", vlan) }

// EvidenceFor computes Evidence for one netns from a Snapshot. Pure function.
func EvidenceFor(s *Snapshot, nn *vitiv1alpha1.NetworkNamespace) Evidence {
	ev := Evidence{IPAllocCount: -1, VlanKnown: nn.Status.VlanID != 0}
	vlanIface := vlanInterfaceName(nn.Status.VlanID)

	liveClusterIDs := make(map[string]bool)
	for i := range s.KCs {
		k := &s.KCs[i]
		if k.Namespace != nn.Namespace {
			continue
		}
		liveClusterIDs[k.Spec.Cluster.ClusterId] = true
		if k.Spec.Cluster.NetworkNamespaceName == nn.Name {
			ev.ReferencingKCs = append(ev.ReferencingKCs, k.Name)
		}
	}

	for i := range s.NCs {
		c := &s.NCs[i]
		if c.Namespace != nn.Namespace {
			continue
		}
		byName := c.Spec.NetworkNamespaceName == nn.Name
		byVlan := false
		if ev.VlanKnown {
			for _, iface := range c.Spec.NetworkInterfaces {
				if iface.Name == vlanIface {
					byVlan = true
					break
				}
			}
		}
		if byName || byVlan {
			ev.NCRefs = append(ev.NCRefs, c.Name)
		}
	}

	if s.IPAllocCRDPresent {
		n := 0
		for i := range s.IPAllocs {
			a := &s.IPAllocs[i]
			if a.GetNamespace() != nn.Namespace {
				continue
			}
			// found/err are honoured rather than discarded: dropping them
			// turns a schema change or a type mismatch into ref == "" for
			// EVERY record, i.e. a hard gate that silently counts zero and
			// reports itself as passed.
			//
			// Treating an unreadable record as blocking cannot produce a
			// false block on a conformant object: the v1alpha2 CRD schema
			// lists networkNamespaceName in spec.required with minLength: 1
			// (verified against the live CRD, 2026-08-17), so the API server
			// rejects any IPAllocation that omits it or leaves it empty.
			// A record that lands in the unevaluated bucket is therefore
			// genuinely anomalous — a schema change, a type mismatch, or a
			// hand-edited object — and is worth refusing on.
			ref, found, err := unstructured.NestedString(a.Object, "spec", "networkNamespaceName")
			switch {
			case err != nil || !found:
				ev.IPAllocUnevaluated = append(ev.IPAllocUnevaluated, a.GetName())
			case ref == nn.Name:
				n++
			}
		}
		ev.IPAllocCount = n
	}

	for _, id := range nn.Status.AssociatedKubernetesClusterIDs {
		if !liveClusterIDs[id] {
			ev.GhostAssocIDs = append(ev.GhostAssocIDs, id)
		}
	}
	return ev
}

// Orphan is a netns nothing claims, plus its full evidence. Deletion may
// still be blocked: an ipallocation whose target could not be read leaves the
// netns unclaimed as far as anything can tell, but not safe to delete.
type Orphan struct {
	NN *vitiv1alpha1.NetworkNamespace
	Ev Evidence
}

// Orphans returns the snapshot's unclaimed netns in input order.
//
// The filter is InUse, not "no referencing KubernetesCluster". Selecting on
// the KC reference alone reported live clusters as orphans: the operator only
// began writing spec.data.networkNamespaceName around 2026-06-02, so a
// cluster created before that names no netns at all. Three such clusters were
// live on ptr1 when this was found (d-trd-atlas-001, t-trd-obao-001,
// d-amk-003, all created February 2026); two of them were alone in their
// namespace and so surfaced as orphans, while the third was masked only by a
// newer sibling cluster that did populate the field.
//
// Their NetworkConfigurations still claimed the vlan<id> interface, so delete
// refused — the audit was wrong, not the gate. Reporting a netns whose VLAN
// is demonstrably carrying machines trains people to ignore the command.
//
// A NetworkConfiguration left behind with no cluster is therefore not
// reported here either: that is drift in a NetworkConfiguration, and calling
// it an orphaned NetworkNamespace would point the reader at the wrong object
// to delete. It belongs in its own audit kind if it is ever worth reporting.
func Orphans(s *Snapshot) []Orphan {
	var out []Orphan
	for i := range s.NetNSs {
		n := &s.NetNSs[i]
		ev := EvidenceFor(s, n)
		if !ev.InUse() {
			out = append(out, Orphan{NN: n, Ev: ev})
		}
	}
	return out
}
