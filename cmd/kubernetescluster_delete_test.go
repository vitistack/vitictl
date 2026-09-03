package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestKCDeleteAndPrecleanAvailabilityZoneFlagsShareGlobalBinding(t *testing.T) {
	deleteAZ := kcDeleteCmd.Flags().Lookup("availabilityzone")
	precleanAZ := kcPrecleanCmd.Flags().Lookup("availabilityzone")
	aliasAZ := rootCmd.PersistentFlags().Lookup("az")
	if deleteAZ == nil || precleanAZ == nil || aliasAZ == nil {
		t.Fatal("delete, preclean, and global --az flags must all be registered")
	}
	if deleteAZ.Shorthand != "z" || precleanAZ.Shorthand != "z" {
		t.Fatal("delete and preclean availability-zone flags must retain the -z shorthand")
	}

	oldAZ := globalAZ
	t.Cleanup(func() { globalAZ = oldAZ })
	for _, tt := range []struct {
		name string
		flag *pflag.Flag
	}{
		{"delete --availabilityzone/-z", deleteAZ},
		{"preclean --availabilityzone/-z", precleanAZ},
		{"global --az", aliasAZ},
	} {
		t.Run(tt.name, func(t *testing.T) {
			globalAZ = ""
			if err := tt.flag.Value.Set("zone-a"); err != nil {
				t.Fatalf("setting flag: %v", err)
			}
			if globalAZ != "zone-a" {
				t.Fatalf("globalAZ = %q, want zone-a", globalAZ)
			}
		})
	}
}

func TestKCDeleteDryRunHelpDoesNotPromiseUnverifiedRORChecks(t *testing.T) {
	usage := kcDeleteCmd.Flags().Lookup("dry-run").Usage
	for _, unsupported := range []string{"token", "registry"} {
		if strings.Contains(strings.ToLower(usage), unsupported) {
			t.Errorf("dry-run help %q must not promise an unimplemented ROR %s check", usage, unsupported)
		}
	}
}
