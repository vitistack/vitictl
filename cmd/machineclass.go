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
})
