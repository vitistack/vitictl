package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
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
		set  func(string) error
	}{
		{"delete --availabilityzone/-z", deleteAZ.Value.Set},
		{"preclean --availabilityzone/-z", precleanAZ.Value.Set},
		{"global --az", aliasAZ.Value.Set},
	} {
		t.Run(tt.name, func(t *testing.T) {
			globalAZ = ""
			if err := tt.set("zone-a"); err != nil {
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

func TestKCDeleteDryRunGuestSetupFailureIsFatal(t *testing.T) {
	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := vitiv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	cluster := &vitiv1alpha1.KubernetesCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "cluster-a"},
	}
	cluster.Spec.Cluster.ClusterId = "cluster-a-id"
	hit := &kcHit{
		client:  &kube.Client{Ctrl: fake.NewClientBuilder().WithScheme(sch).Build()},
		cluster: cluster,
	}
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)

	oldDryRun, oldSkip := kcDeleteDryRun, kcDeleteSkipPreclean
	kcDeleteDryRun, kcDeleteSkipPreclean = true, false
	t.Cleanup(func() {
		kcDeleteDryRun, kcDeleteSkipPreclean = oldDryRun, oldSkip
	})

	if _, err := newKCDeleteRunner(t.Context(), cmd, hit); err == nil {
		t.Fatal("dry-run guest setup failure must return an error")
	}
	if strings.Contains(out.String(), "preflight clean") {
		t.Fatalf("output = %q, must not print a clean verdict after guest setup fails", out.String())
	}
}
