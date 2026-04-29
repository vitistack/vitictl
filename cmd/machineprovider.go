package cmd

import (
	"fmt"
	"strings"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/printer"
)

var machineProviderCmd = buildResourceCmd(resourceBinding[*vitiv1alpha1.MachineProvider, *vitiv1alpha1.MachineProviderList]{
	Use:        "machineprovider",
	Aliases:    []string{"mp", "machineproviders"},
	Short:      "🏭 Work with MachineProvider resources",
	Namespaced: false,
	NameKind:   "machineprovider",
	NewList:    func() *vitiv1alpha1.MachineProviderList { return &vitiv1alpha1.MachineProviderList{} },
	Items: func(l *vitiv1alpha1.MachineProviderList) []*vitiv1alpha1.MachineProvider {
		return itemsOf(l.Items)
	},
	Headers: func(wide bool) string {
		if wide {
			return "AZ\tNAME\tDISPLAY NAME\tTYPE\tREGION\tPHASE\tACTIVE\tHEALTH\tAGE"
		}
		return "AZ\tNAME\tTYPE\tREGION\tPHASE\tACTIVE"
	},
	Row: func(az string, o *vitiv1alpha1.MachineProvider, wide bool) string {
		if wide {
			return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s",
				az, o.Name, o.Spec.DisplayName, o.Spec.ProviderType,
				o.Spec.Region, o.Status.Phase, o.Status.ActiveMachines,
				valueOrDash(o.Status.Health.Status), printer.Age(o.CreationTimestamp))
		}
		return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%d",
			az, o.Name, o.Spec.ProviderType, o.Spec.Region, o.Status.Phase, o.Status.ActiveMachines)
	},
	SearchLabel: func(az string, o *vitiv1alpha1.MachineProvider) string {
		return strings.Join([]string{az, o.Name, o.Spec.DisplayName, o.Spec.ProviderType, o.Spec.Region}, " ")
	},
	SortKeys: map[string]func(a, b *vitiv1alpha1.MachineProvider) int{
		"display-name": func(a, b *vitiv1alpha1.MachineProvider) int {
			return strings.Compare(a.Spec.DisplayName, b.Spec.DisplayName)
		},
		"type": func(a, b *vitiv1alpha1.MachineProvider) int {
			return strings.Compare(a.Spec.ProviderType, b.Spec.ProviderType)
		},
		"region": func(a, b *vitiv1alpha1.MachineProvider) int { return strings.Compare(a.Spec.Region, b.Spec.Region) },
		"phase":  func(a, b *vitiv1alpha1.MachineProvider) int { return strings.Compare(a.Status.Phase, b.Status.Phase) },
		"active": func(a, b *vitiv1alpha1.MachineProvider) int {
			switch {
			case a.Status.ActiveMachines < b.Status.ActiveMachines:
				return -1
			case a.Status.ActiveMachines > b.Status.ActiveMachines:
				return 1
			}
			return 0
		},
		"health": func(a, b *vitiv1alpha1.MachineProvider) int {
			return strings.Compare(a.Status.Health.Status, b.Status.Health.Status)
		},
	},
})
