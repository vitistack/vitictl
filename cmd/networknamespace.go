package cmd

import (
	"fmt"
	"strings"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/printer"
)

var networkNamespaceCmd = buildResourceCmd(resourceBinding[*vitiv1alpha1.NetworkNamespace, *vitiv1alpha1.NetworkNamespaceList]{
	Use:        "networknamespace",
	Aliases:    []string{"nn", "networknamespaces"},
	Short:      "🕸️  Work with NetworkNamespace resources",
	Namespaced: true,
	NameKind:   "networknamespace",
	NewList:    func() *vitiv1alpha1.NetworkNamespaceList { return &vitiv1alpha1.NetworkNamespaceList{} },
	Items: func(l *vitiv1alpha1.NetworkNamespaceList) []*vitiv1alpha1.NetworkNamespace {
		return itemsOf(l.Items)
	},
	Headers: func(wide bool) string {
		if wide {
			return "AZ\tNAMESPACE\tNAME\tDATACENTER\tSUPERVISOR\tPHASE\tVLAN\tIPV4 PREFIX\tIPV6 PREFIX\tNS ID\tAGE"
		}
		return "AZ\tNAMESPACE\tNAME\tDATACENTER\tPHASE\tVLAN\tIPV4 PREFIX"
	},
	Row: func(az string, o *vitiv1alpha1.NetworkNamespace, wide bool) string {
		if wide {
			return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s",
				az, o.Namespace, o.Name,
				valueOrDash(o.Spec.DatacenterIdentifier), valueOrDash(o.Spec.SupervisorIdentifier),
				o.Status.Phase, o.Status.VlanID,
				valueOrDash(o.Status.IPv4Prefix), valueOrDash(o.Status.IPv6Prefix),
				valueOrDash(o.Status.NamespaceID), printer.Age(o.CreationTimestamp))
		}
		return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%d\t%s",
			az, o.Namespace, o.Name, valueOrDash(o.Spec.DatacenterIdentifier),
			o.Status.Phase, o.Status.VlanID, valueOrDash(o.Status.IPv4Prefix))
	},
	SearchLabel: func(az string, o *vitiv1alpha1.NetworkNamespace) string {
		return strings.Join([]string{az, o.Namespace, o.Name, o.Spec.DatacenterIdentifier, o.Spec.SupervisorIdentifier, o.Status.NamespaceID}, " ")
	},
	SortKeys: map[string]func(a, b *vitiv1alpha1.NetworkNamespace) int{
		"datacenter": func(a, b *vitiv1alpha1.NetworkNamespace) int {
			return strings.Compare(a.Spec.DatacenterIdentifier, b.Spec.DatacenterIdentifier)
		},
		"supervisor": func(a, b *vitiv1alpha1.NetworkNamespace) int {
			return strings.Compare(a.Spec.SupervisorIdentifier, b.Spec.SupervisorIdentifier)
		},
		"phase": func(a, b *vitiv1alpha1.NetworkNamespace) int { return strings.Compare(a.Status.Phase, b.Status.Phase) },
		"vlan": func(a, b *vitiv1alpha1.NetworkNamespace) int {
			switch {
			case a.Status.VlanID < b.Status.VlanID:
				return -1
			case a.Status.VlanID > b.Status.VlanID:
				return 1
			}
			return 0
		},
		"ipv4-prefix": func(a, b *vitiv1alpha1.NetworkNamespace) int {
			return strings.Compare(a.Status.IPv4Prefix, b.Status.IPv4Prefix)
		},
		"ipv6-prefix": func(a, b *vitiv1alpha1.NetworkNamespace) int {
			return strings.Compare(a.Status.IPv6Prefix, b.Status.IPv6Prefix)
		},
		"ns-id": func(a, b *vitiv1alpha1.NetworkNamespace) int {
			return strings.Compare(a.Status.NamespaceID, b.Status.NamespaceID)
		},
	},
})
