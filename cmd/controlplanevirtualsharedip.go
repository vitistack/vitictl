package cmd

import (
	"fmt"
	"strings"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/printer"
)

var controlPlaneVirtualSharedIPCmd = buildResourceCmd(resourceBinding[*vitiv1alpha1.ControlPlaneVirtualSharedIP, *vitiv1alpha1.ControlPlaneVirtualSharedIPList]{
	Use:        "controlplanevirtualsharedip",
	Aliases:    []string{"lb", "controlplanevirtualsharedips", "cpvip"},
	Short:      "🧷 Work with ControlPlaneVirtualSharedIP (load balancer) resources",
	Namespaced: true,
	NameKind:   "controlplanevirtualsharedip",
	NewList: func() *vitiv1alpha1.ControlPlaneVirtualSharedIPList {
		return &vitiv1alpha1.ControlPlaneVirtualSharedIPList{}
	},
	Items: func(l *vitiv1alpha1.ControlPlaneVirtualSharedIPList) []*vitiv1alpha1.ControlPlaneVirtualSharedIP {
		return itemsOf(l.Items)
	},
	Headers: func(wide bool) string {
		if wide {
			return "AZ\tNAMESPACE\tNAME\tPROVIDER\tCLUSTER\tDATACENTER\tMETHOD\tPHASE\tSTATUS\tLB IPS\tAGE"
		}
		return "AZ\tNAMESPACE\tNAME\tPROVIDER\tCLUSTER\tPHASE\tLB IPS"
	},
	Row: func(az string, o *vitiv1alpha1.ControlPlaneVirtualSharedIP, wide bool) string {
		ips := strings.Join(o.Status.LoadBalancerIps, ",")
		if wide {
			return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
				az, o.Namespace, o.Name,
				valueOrDash(o.Spec.Provider), valueOrDash(o.Spec.ClusterIdentifier),
				valueOrDash(o.Spec.DatacenterIdentifier), valueOrDash(o.Spec.Method),
				o.Status.Phase, valueOrDash(o.Status.Status),
				valueOrDash(ips), printer.Age(o.CreationTimestamp))
		}
		return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s",
			az, o.Namespace, o.Name, valueOrDash(o.Spec.Provider),
			valueOrDash(o.Spec.ClusterIdentifier), o.Status.Phase, valueOrDash(ips))
	},
	SearchLabel: func(az string, o *vitiv1alpha1.ControlPlaneVirtualSharedIP) string {
		return strings.Join([]string{
			az, o.Namespace, o.Name, o.Spec.Provider, o.Spec.ClusterIdentifier,
			o.Spec.DatacenterIdentifier, strings.Join(o.Status.LoadBalancerIps, " "),
		}, " ")
	},
})
