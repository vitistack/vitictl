package cmd

import (
	"fmt"
	"strings"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/printer"
)

var kubevirtConfigCmd = buildResourceCmd(resourceBinding[*vitiv1alpha1.KubevirtConfig, *vitiv1alpha1.KubevirtConfigList]{
	Use:        "kubevirtconfig",
	Aliases:    []string{"kvc", "kubevirtconfigs"},
	Short:      "💻 Work with KubevirtConfig resources",
	Namespaced: false,
	NameKind:   "kubevirtconfig",
	NewList:    func() *vitiv1alpha1.KubevirtConfigList { return &vitiv1alpha1.KubevirtConfigList{} },
	Items: func(l *vitiv1alpha1.KubevirtConfigList) []*vitiv1alpha1.KubevirtConfig {
		return itemsOf(l.Items)
	},
	Headers: func(wide bool) string {
		if wide {
			return "AZ\tNAME\tSPEC NAME\tSECRET NS\tSECRET REF\tPHASE\tSTATUS\tAGE"
		}
		return "AZ\tNAME\tSPEC NAME\tSECRET NS\tPHASE"
	},
	Row: func(az string, o *vitiv1alpha1.KubevirtConfig, wide bool) string {
		if wide {
			return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
				az, o.Name, valueOrDash(o.Spec.Name),
				valueOrDash(o.Spec.SecretNamespace), valueOrDash(o.Spec.KubeconfigSecretRef),
				valueOrDash(o.Status.Phase), valueOrDash(o.Status.Status),
				printer.Age(o.CreationTimestamp))
		}
		return fmt.Sprintf("%s\t%s\t%s\t%s\t%s",
			az, o.Name, valueOrDash(o.Spec.Name), valueOrDash(o.Spec.SecretNamespace), valueOrDash(o.Status.Phase))
	},
	SearchLabel: func(az string, o *vitiv1alpha1.KubevirtConfig) string {
		return strings.Join([]string{az, o.Name, o.Spec.Name, o.Spec.SecretNamespace, o.Spec.KubeconfigSecretRef}, " ")
	},
})
