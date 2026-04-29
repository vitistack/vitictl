package cmd

import (
	"fmt"
	"strings"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/printer"
)

var clusterStorageCmd = buildResourceCmd(resourceBinding[*vitiv1alpha1.ClusterStorage, *vitiv1alpha1.ClusterStorageList]{
	Use:        "clusterstorage",
	Aliases:    []string{"cls", "clusterstorages"},
	Short:      "🗄️  Work with ClusterStorage resources",
	Namespaced: true,
	NameKind:   "clusterstorage",
	NewList:    func() *vitiv1alpha1.ClusterStorageList { return &vitiv1alpha1.ClusterStorageList{} },
	Items: func(l *vitiv1alpha1.ClusterStorageList) []*vitiv1alpha1.ClusterStorage {
		return itemsOf(l.Items)
	},
	Headers: func(wide bool) string {
		if wide {
			return "AZ\tNAMESPACE\tNAME\tCLUSTER ID\tTYPE\tSTORAGE CLASS\tREUSE\tEXISTING REF\tPHASE\tSECRET\tGUEST\tAGE"
		}
		return "AZ\tNAMESPACE\tNAME\tCLUSTER ID\tTYPE\tSTORAGE CLASS\tPHASE"
	},
	Row: func(az string, o *vitiv1alpha1.ClusterStorage, wide bool) string {
		if wide {
			return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%v\t%s\t%s\t%s\t%s\t%s",
				az, o.Namespace, o.Name,
				valueOrDash(o.Spec.ClusterId), valueOrDash(o.Spec.Type),
				valueOrDash(o.Spec.ClusterStorageClass), o.Spec.ReuseExisting,
				valueOrDash(o.Spec.ExistingRef),
				valueOrDash(o.Status.Phase),
				valueOrDash(o.Status.Secret.Condition),
				valueOrDash(o.Status.GuestResource.Condition),
				printer.Age(o.CreationTimestamp))
		}
		return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s",
			az, o.Namespace, o.Name,
			valueOrDash(o.Spec.ClusterId), valueOrDash(o.Spec.Type),
			valueOrDash(o.Spec.ClusterStorageClass), valueOrDash(o.Status.Phase))
	},
	SearchLabel: func(az string, o *vitiv1alpha1.ClusterStorage) string {
		return strings.Join([]string{
			az, o.Namespace, o.Name,
			o.Spec.ClusterId, o.Spec.Type, o.Spec.ClusterStorageClass, o.Spec.ExistingRef,
		}, " ")
	},
	SortKeys: map[string]func(a, b *vitiv1alpha1.ClusterStorage) int{
		"cluster-id": func(a, b *vitiv1alpha1.ClusterStorage) int {
			return strings.Compare(a.Spec.ClusterId, b.Spec.ClusterId)
		},
		"type": func(a, b *vitiv1alpha1.ClusterStorage) int { return strings.Compare(a.Spec.Type, b.Spec.Type) },
		"storage-class": func(a, b *vitiv1alpha1.ClusterStorage) int {
			return strings.Compare(a.Spec.ClusterStorageClass, b.Spec.ClusterStorageClass)
		},
		"phase": func(a, b *vitiv1alpha1.ClusterStorage) int { return strings.Compare(a.Status.Phase, b.Status.Phase) },
	},
})
