package cmd

import (
	"fmt"
	"strings"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/printer"
)

var proxmoxConfigCmd = buildResourceCmd(resourceBinding[*vitiv1alpha1.ProxmoxConfig, *vitiv1alpha1.ProxmoxConfigList]{
	Use:        "proxmoxconfig",
	Aliases:    []string{"pxc", "proxmoxconfigs"},
	Short:      "🔌 Work with ProxmoxConfig resources",
	Namespaced: false,
	NameKind:   "proxmoxconfig",
	NewList:    func() *vitiv1alpha1.ProxmoxConfigList { return &vitiv1alpha1.ProxmoxConfigList{} },
	Items: func(l *vitiv1alpha1.ProxmoxConfigList) []*vitiv1alpha1.ProxmoxConfig {
		return itemsOf(l.Items)
	},
	Headers: func(wide bool) string {
		if wide {
			return "AZ\tNAME\tSPEC NAME\tENDPOINT\tPORT\tUSERNAME\tPHASE\tSTATUS\tAGE"
		}
		return "AZ\tNAME\tENDPOINT\tPORT\tPHASE"
	},
	Row: func(az string, o *vitiv1alpha1.ProxmoxConfig, wide bool) string {
		if wide {
			return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
				az, o.Name, valueOrDash(o.Spec.Name),
				valueOrDash(o.Spec.Endpoint), valueOrDash(o.Spec.Port),
				valueOrDash(o.Spec.Username),
				valueOrDash(o.Status.Phase), valueOrDash(o.Status.Status),
				printer.Age(o.CreationTimestamp))
		}
		return fmt.Sprintf("%s\t%s\t%s\t%s\t%s",
			az, o.Name, valueOrDash(o.Spec.Endpoint), valueOrDash(o.Spec.Port), valueOrDash(o.Status.Phase))
	},
	SearchLabel: func(az string, o *vitiv1alpha1.ProxmoxConfig) string {
		return strings.Join([]string{az, o.Name, o.Spec.Name, o.Spec.Endpoint, o.Spec.Username}, " ")
	},
})
