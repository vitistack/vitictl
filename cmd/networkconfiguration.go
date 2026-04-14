package cmd

import (
	"fmt"
	"strings"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/printer"
)

var networkConfigurationCmd = buildResourceCmd(resourceBinding[*vitiv1alpha1.NetworkConfiguration, *vitiv1alpha1.NetworkConfigurationList]{
	Use:        "networkconfiguration",
	Aliases:    []string{"nc", "networkconfigurations"},
	Short:      "🌐 Work with NetworkConfiguration resources",
	Namespaced: true,
	NameKind:   "networkconfiguration",
	NewList:    func() *vitiv1alpha1.NetworkConfigurationList { return &vitiv1alpha1.NetworkConfigurationList{} },
	Items: func(l *vitiv1alpha1.NetworkConfigurationList) []*vitiv1alpha1.NetworkConfiguration {
		return itemsOf(l.Items)
	},
	Headers: func(wide bool) string {
		if wide {
			return "AZ\tNAMESPACE\tNAME\tPROVIDER\tDATACENTER\tCLUSTER\tNN\t#IFACES\tPHASE\tSTATUS\tAGE"
		}
		return "AZ\tNAMESPACE\tNAME\tPROVIDER\tDATACENTER\tPHASE"
	},
	Row: func(az string, o *vitiv1alpha1.NetworkConfiguration, wide bool) string {
		if wide {
			return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s",
				az, o.Namespace, o.Name,
				valueOrDash(o.Spec.Provider), valueOrDash(o.Spec.DatacenterIdentifier),
				valueOrDash(o.Spec.ClusterIdentifier), valueOrDash(o.Spec.NetworkNamespaceName),
				len(o.Spec.NetworkInterfaces), o.Status.Phase, valueOrDash(o.Status.Status),
				printer.Age(o.CreationTimestamp))
		}
		return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s",
			az, o.Namespace, o.Name, valueOrDash(o.Spec.Provider),
			valueOrDash(o.Spec.DatacenterIdentifier), o.Status.Phase)
	},
	SearchLabel: func(az string, o *vitiv1alpha1.NetworkConfiguration) string {
		return strings.Join([]string{az, o.Namespace, o.Name, o.Spec.Name, o.Spec.Provider, o.Spec.DatacenterIdentifier, o.Spec.ClusterIdentifier}, " ")
	},
})
