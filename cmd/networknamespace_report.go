package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"sigs.k8s.io/yaml"

	"github.com/vitistack/vitictl/internal/netns"
	"github.com/vitistack/vitictl/internal/printer"
)

// orphanReport is one row of `nn orphans` in machine-readable form.
//
// This is deliberately not the NetworkNamespace object: the useful part of an
// audit is the evidence (what still references it, what could not be
// evaluated), which lives nowhere in the CR. Emitting the CR would hand
// callers the one thing they can already get from `nn get`.
type orphanReport struct {
	AvailabilityZone string `json:"availabilityZone"`
	Namespace        string `json:"namespace"`
	Name             string `json:"name"`
	Phase            string `json:"phase,omitempty"`
	Age              string `json:"age,omitempty"`
	VlanID           int    `json:"vlanId"`
	IPv4Prefix       string `json:"ipv4Prefix,omitempty"`
	IPv6Prefix       string `json:"ipv6Prefix,omitempty"`
	IPv4EgressIP     string `json:"ipv4EgressIp,omitempty"`
	// No NetworkConfiguration refs: a bound NetworkConfiguration disqualifies
	// the netns from being an orphan at all (netns.Orphans), so the field
	// could only ever be empty. Carrying it would invite a future reader to
	// "fix" the renderer to display a value that cannot exist. NCs whose
	// binding is UNKNOWN are a different matter and are reported below.
	NCUnevaluated []string `json:"networkConfigurationsUnevaluated,omitempty"`
	// IPAllocCount is -1 when the IPAllocation CRD is not installed on the
	// zone, which is not the same as zero and must stay distinguishable.
	IPAllocCount       int      `json:"ipAllocationCount"`
	IPAllocUnevaluated []string `json:"ipAllocationsUnevaluated,omitempty"`
	GhostAssocIDs      []string `json:"staleStatusAssociations,omitempty"`
	// Deletable mirrors the gates `nn delete` applies, so a caller can filter
	// candidates without re-deriving the rules.
	Deletable bool `json:"deletable"`

	// ipAllocCell is the pre-rendered table cell; unexported so it never
	// reaches structured output, where the numeric fields are authoritative.
	ipAllocCell string `json:"-"`
}

// orphanAudit wraps the records with the coverage numbers. The counts travel
// with the payload so a partial audit stays detectable downstream of a pipe,
// where the stderr warnings and the exit code have both been lost.
type orphanAudit struct {
	Orphans         []orphanReport `json:"orphans"`
	ZonesAudited    int            `json:"zonesAudited"`
	ZonesConfigured int            `json:"zonesConfigured"`
	Complete        bool           `json:"complete"`
}

func newOrphanReport(azName string, o netns.Orphan) orphanReport {
	ev := o.Ev
	return orphanReport{
		AvailabilityZone:   azName,
		Namespace:          o.NN.Namespace,
		Name:               o.NN.Name,
		Phase:              o.NN.Status.Phase,
		Age:                printer.Age(o.NN.CreationTimestamp),
		VlanID:             o.NN.Status.VlanID,
		IPv4Prefix:         o.NN.Status.IPv4Prefix,
		IPv6Prefix:         o.NN.Status.IPv6Prefix,
		IPv4EgressIP:       o.NN.Status.IPv4EgressIP,
		NCUnevaluated:      ev.NCUnevaluated,
		IPAllocCount:       ev.IPAllocCount,
		IPAllocUnevaluated: ev.IPAllocUnevaluated,
		GhostAssocIDs:      ev.GhostAssocIDs,
		Deletable:          !ev.Blocked(),
		ipAllocCell:        ipAllocCell(ev),
	}
}

// ipAllocCell renders the IPALLOCS table cell: "n/a" where the CRD is absent,
// otherwise the count, with "+N?" appended for records whose target could not
// be determined.
func ipAllocCell(ev netns.Evidence) string {
	cell := "n/a"
	if ev.IPAllocCount >= 0 {
		cell = strconv.Itoa(ev.IPAllocCount)
	}
	if n := len(ev.IPAllocUnevaluated); n > 0 {
		cell += "+" + strconv.Itoa(n) + "?"
	}
	return cell
}

func writeOrphanAudit(w io.Writer, format printer.Format, report orphanAudit) error {
	report.Complete = report.ZonesAudited == report.ZonesConfigured
	switch format {
	case printer.FormatYAML:
		raw, err := yaml.Marshal(report)
		if err != nil {
			return fmt.Errorf("encoding orphan audit as yaml: %w", err)
		}
		_, err = w.Write(raw)
		return err
	default:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("encoding orphan audit as json: %w", err)
		}
		return nil
	}
}
