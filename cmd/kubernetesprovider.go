package cmd

import (
	"fmt"
	"strings"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/printer"
)

var kubernetesProviderCmd = buildResourceCmd(resourceBinding[*vitiv1alpha1.KubernetesProvider, *vitiv1alpha1.KubernetesProviderList]{
	Use:        "kubernetesprovider",
	Aliases:    []string{"kp", "kubernetesproviders"},
	Short:      "☁️  Work with KubernetesProvider resources",
	Namespaced: false,
	NameKind:   "kubernetesprovider",
	NewList:    func() *vitiv1alpha1.KubernetesProviderList { return &vitiv1alpha1.KubernetesProviderList{} },
	Items: func(l *vitiv1alpha1.KubernetesProviderList) []*vitiv1alpha1.KubernetesProvider {
		return itemsOf(l.Items)
	},
	Headers: func(wide bool) string {
		if wide {
			return "AZ\tNAME\tDISPLAY NAME\tTYPE\tVERSION\tREGION\tPHASE\tNODES\tREADY\tAGE"
		}
		return "AZ\tNAME\tTYPE\tVERSION\tREGION\tPHASE\tNODES"
	},
	Row: func(az string, o *vitiv1alpha1.KubernetesProvider, wide bool) string {
		if wide {
			return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s",
				az, o.Name, o.Spec.DisplayName, o.Spec.ProviderType, o.Spec.Version,
				o.Spec.Region, o.Status.Phase, o.Status.NodeCount, o.Status.ReadyNodeCount,
				printer.Age(o.CreationTimestamp))
		}
		return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%d",
			az, o.Name, o.Spec.ProviderType, o.Spec.Version, o.Spec.Region, o.Status.Phase, o.Status.NodeCount)
	},
	SearchLabel: func(az string, o *vitiv1alpha1.KubernetesProvider) string {
		return strings.Join([]string{az, o.Name, o.Spec.DisplayName, o.Spec.ProviderType, o.Spec.Region, o.Spec.Version}, " ")
	},
})
