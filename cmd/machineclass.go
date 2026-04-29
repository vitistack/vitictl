package cmd

import (
	"fmt"
	"strings"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/printer"
)

var machineClassCmd = buildResourceCmd(resourceBinding[*vitiv1alpha1.MachineClass, *vitiv1alpha1.MachineClassList]{
	Use:        "machineclass",
	Aliases:    []string{"mc", "machineclasses"},
	Short:      "🧩 Work with MachineClass resources",
	Namespaced: false,
	NameKind:   "machineclass",
	NewList:    func() *vitiv1alpha1.MachineClassList { return &vitiv1alpha1.MachineClassList{} },
	Items: func(l *vitiv1alpha1.MachineClassList) []*vitiv1alpha1.MachineClass {
		return itemsOf(l.Items)
	},
	Headers: func(wide bool) string {
		if wide {
			return "AZ\tNAME\tDISPLAY NAME\tCATEGORY\tCPU\tMEMORY\tGPU\tENABLED\tDEFAULT\tPHASE\tAGE"
		}
		return "AZ\tNAME\tCATEGORY\tCPU\tMEMORY\tENABLED\tPHASE"
	},
	Row: func(az string, o *vitiv1alpha1.MachineClass, wide bool) string {
		mem := o.Spec.Memory.Quantity.String()
		if wide {
			return fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%s\t%d\t%v\t%v\t%s\t%s",
				az, o.Name, o.Spec.DisplayName, o.Spec.Category,
				o.Spec.CPU.Cores, mem, o.Spec.GPU.Cores,
				o.Spec.Enabled, o.Spec.Default, o.Status.Phase,
				printer.Age(o.CreationTimestamp))
		}
		return fmt.Sprintf("%s\t%s\t%s\t%d\t%s\t%v\t%s",
			az, o.Name, o.Spec.Category, o.Spec.CPU.Cores, mem, o.Spec.Enabled, o.Status.Phase)
	},
	SearchLabel: func(az string, o *vitiv1alpha1.MachineClass) string {
		return strings.Join([]string{az, o.Name, o.Spec.DisplayName, o.Spec.Category}, " ")
	},
	SortKeys: map[string]func(a, b *vitiv1alpha1.MachineClass) int{
		"display-name": func(a, b *vitiv1alpha1.MachineClass) int {
			return strings.Compare(a.Spec.DisplayName, b.Spec.DisplayName)
		},
		"category": func(a, b *vitiv1alpha1.MachineClass) int { return strings.Compare(a.Spec.Category, b.Spec.Category) },
		"phase":    func(a, b *vitiv1alpha1.MachineClass) int { return strings.Compare(a.Status.Phase, b.Status.Phase) },
		"cpu": func(a, b *vitiv1alpha1.MachineClass) int {
			switch {
			case a.Spec.CPU.Cores < b.Spec.CPU.Cores:
				return -1
			case a.Spec.CPU.Cores > b.Spec.CPU.Cores:
				return 1
			}
			return 0
		},
		"memory": func(a, b *vitiv1alpha1.MachineClass) int { return a.Spec.Memory.Quantity.Cmp(b.Spec.Memory.Quantity) },
		"gpu": func(a, b *vitiv1alpha1.MachineClass) int {
			switch {
			case a.Spec.GPU.Cores < b.Spec.GPU.Cores:
				return -1
			case a.Spec.GPU.Cores > b.Spec.GPU.Cores:
				return 1
			}
			return 0
		},
		"enabled": func(a, b *vitiv1alpha1.MachineClass) int { return cmpBool(a.Spec.Enabled, b.Spec.Enabled) },
		"default": func(a, b *vitiv1alpha1.MachineClass) int { return cmpBool(a.Spec.Default, b.Spec.Default) },
	},
})
