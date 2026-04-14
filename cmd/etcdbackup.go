package cmd

import (
	"fmt"
	"strings"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/printer"
)

var etcdBackupCmd = buildResourceCmd(resourceBinding[*vitiv1alpha1.EtcdBackup, *vitiv1alpha1.EtcdBackupList]{
	Use:        "etcdbackup",
	Aliases:    []string{"eb", "etcdbackups"},
	Short:      "💾 Work with EtcdBackup resources",
	Namespaced: true,
	NameKind:   "etcdbackup",
	NewList:    func() *vitiv1alpha1.EtcdBackupList { return &vitiv1alpha1.EtcdBackupList{} },
	Items: func(l *vitiv1alpha1.EtcdBackupList) []*vitiv1alpha1.EtcdBackup {
		return itemsOf(l.Items)
	},
	Headers: func(wide bool) string {
		if wide {
			return "AZ\tNAMESPACE\tNAME\tCLUSTER\tSTORAGE\tSCHEDULE\tRETENTION\tPHASE\tCOUNT\tSIZE\tLAST BACKUP\tAGE"
		}
		return "AZ\tNAMESPACE\tNAME\tCLUSTER\tSTORAGE\tPHASE\tLAST BACKUP"
	},
	Row: func(az string, o *vitiv1alpha1.EtcdBackup, wide bool) string {
		last := "-"
		if o.Status.LastBackupTime != nil {
			last = printer.Age(*o.Status.LastBackupTime)
		}
		if wide {
			return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%d\t%s\t%s\t%s",
				az, o.Namespace, o.Name,
				valueOrDash(o.Spec.ClusterName), valueOrDash(o.Spec.StorageLocation.Type),
				valueOrDash(o.Spec.Schedule), o.Spec.Retention,
				valueOrDash(o.Status.Phase), o.Status.BackupCount,
				valueOrDash(o.Status.BackupSize), last,
				printer.Age(o.CreationTimestamp))
		}
		return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s",
			az, o.Namespace, o.Name,
			valueOrDash(o.Spec.ClusterName), valueOrDash(o.Spec.StorageLocation.Type),
			valueOrDash(o.Status.Phase), last)
	},
	SearchLabel: func(az string, o *vitiv1alpha1.EtcdBackup) string {
		return strings.Join([]string{az, o.Namespace, o.Name, o.Spec.ClusterName, o.Spec.StorageLocation.Type}, " ")
	},
})
