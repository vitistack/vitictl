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
	deleteAZ := kcDeleteCmd.Flag("availabilityzone")
	precleanAZ := kcPrecleanCmd.Flag("availabilityzone")
	rootAZ := rootCmd.PersistentFlags().Lookup("availabilityzone")
	aliasAZ := rootCmd.PersistentFlags().Lookup("az")
	if deleteAZ == nil || precleanAZ == nil || rootAZ == nil || aliasAZ == nil {
		t.Fatal("delete, preclean, and global --az flags must all be registered")
	}
	if deleteAZ.Shorthand != "z" || precleanAZ.Shorthand != "z" {
		t.Fatal("delete and preclean availability-zone flags must retain the -z shorthand")
	}
	if kcDeleteCmd.Flags().Lookup("availabilityzone") != nil || kcPrecleanCmd.Flags().Lookup("availabilityzone") != nil {
		t.Fatal("delete and preclean must inherit --availabilityzone rather than shadowing the global flag")
	}

	oldAZ := globalAZ
	t.Cleanup(func() { globalAZ = oldAZ })
	for _, tt := range []struct {
		name string
		set  func(string) error
	}{
		{"global --availabilityzone/-z", rootAZ.Value.Set},
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
		client:  &kube.Client{Ctrl: fake.NewClientBuilder().WithScheme(sch).WithObjects(cluster).Build()},
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

	runner, err := newKCDeleteRunner(t.Context(), cmd, hit)
	if err != nil {
		t.Fatalf("dry-run runner setup = %v, want Preflight to aggregate the failure", err)
	}
	if err := runner.Preflight(t.Context()); err == nil {
		t.Fatal("dry-run guest setup failure must make preflight return an error")
	}
	got := out.String()
	for _, want := range []string{"KubernetesCluster team-a/cluster-a", "machines: 0 found", "guest client unavailable"} {
		if !strings.Contains(got, want) {
			t.Errorf("output = %q, want aggregate check %q", got, want)
		}
	}
	if strings.Contains(got, "preflight clean") {
		t.Fatalf("output = %q, must not print a clean verdict after guest setup fails", got)
	}
}
