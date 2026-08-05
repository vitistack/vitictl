package cmd

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/decommission"
	"github.com/vitistack/vitictl/internal/extract"
	"github.com/vitistack/vitictl/internal/kube"
)

var (
	kcDeleteAZ             string
	kcDeleteNamespace      string
	kcDeleteYes            bool
	kcDeleteSkipPreclean   bool
	kcDeleteMachineTimeout time.Duration
	kcDeleteRORURL         string
)

var kcDeleteCmd = &cobra.Command{
	Use:     "delete <name>",
	Aliases: []string{"decommission"},
	Short:   "Fully decommission a KubernetesCluster (DESTRUCTIVE)",
	Long: `Fully decommissions a guest cluster in one go:

Phase 1 — preclean the guest (via kubeconfig extracted from the cluster's
own secret): stop ArgoCD, delete ingresses/gateways/LoadBalancer services
(so IPAM and DNS operators release external state), delete PVCs and wait
until the backing volumes are gone from external storage, and purge the
cluster from the ROR registry.

Phase 2 — only if phase 1 is verifiably clean: delete the KubernetesCluster
CR and watch the operator teardown (VMs, network configuration, API VIP,
node-IP allocations) until everything is verifiably gone.

⚠️  THIS IS IRREVERSIBLE once phase 2 starts. Finalizers are never stripped:
they are the cleanup mechanism. If teardown hangs, investigate the vitistack
operators on the management cluster.

Phase 1 requires a fresh 'ror login' (expired tokens fail loudly, not
silently). Pass --skip-preclean only for a guest that is unreachable or
already precleaned — external state held by the guest is NOT cleaned then.
Pass --yes to skip the confirmation prompt.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		ctx := context.Background()

		zones, err := kube.ResolveAvailabilityZones(kcDeleteAZ)
		if err != nil {
			return err
		}
		clients, err := kube.ConnectAll(ctx, zones, true, func(err error) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "⚠️  %v\n", err)
		})
		if err != nil {
			return err
		}
		hit, err := findClusterAcrossAZs(ctx, clients, name, kcDeleteNamespace)
		if err != nil {
			return err
		}

		var guest ctrlclient.Client
		if !kcDeleteSkipPreclean {
			guest, err = buildGuestClient(ctx, hit)
			if err != nil {
				return fmt.Errorf("cannot reach the guest cluster for preclean: %w\n"+
					"If the guest is genuinely gone/unreachable and you accept leaking its external state, re-run with --skip-preclean", err)
			}
		}

		printKcDeleteSummary(cmd, hit)
		if !kcDeleteYes {
			if err := confirmKcDelete(cmd, name); err != nil {
				return err
			}
		}

		runner, err := decommission.New(hit.client.Ctrl, guest, hit.cluster, decommission.Options{
			Out:            cmd.OutOrStdout(),
			RORAPIURL:      kcDeleteRORURL,
			SkipPreclean:   kcDeleteSkipPreclean,
			MachineTimeout: kcDeleteMachineTimeout,
		})
		if err != nil {
			return err
		}
		return runner.Run(ctx)
	},
}

// buildGuestClient constructs an in-memory client for the guest cluster from
// the kubeconfig stored in the cluster's own secret on the management
// cluster. Deriving guest access from the CR being deleted makes
// wrong-cluster pairings impossible by construction.
func buildGuestClient(ctx context.Context, hit *kcHit) (ctrlclient.Client, error) {
	secret, err := extract.FindClusterSecret(ctx, hit.client.Ctrl, hit.cluster)
	if err != nil {
		return nil, err
	}
	kubeconfig, ok := secret.Data[extract.KeyKubeConfig]
	if !ok || len(kubeconfig) == 0 {
		return nil, fmt.Errorf("secret %s/%s has no %s", secret.Namespace, secret.Name, extract.KeyKubeConfig)
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("building guest REST config: %w", err)
	}
	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		return nil, err
	}
	if err := apiextv1.AddToScheme(sch); err != nil {
		return nil, err
	}
	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: sch})
	if err != nil {
		return nil, fmt.Errorf("building guest client: %w", err)
	}
	return c, nil
}

func printKcDeleteSummary(cmd *cobra.Command, hit *kcHit) {
	out := cmd.OutOrStdout()
	clusterID := hit.cluster.Spec.Cluster.ClusterId
	_, _ = fmt.Fprintf(out, "\nAbout to DECOMMISSION guest cluster:\n")
	_, _ = fmt.Fprintf(out, "  availability zone : %s\n", hit.client.AZ.Name)
	_, _ = fmt.Fprintf(out, "  namespace         : %s\n", hit.cluster.Namespace)
	_, _ = fmt.Fprintf(out, "  cluster           : %s (clusterId: %s, phase: %s)\n",
		hit.cluster.Name, clusterID, hit.cluster.Status.Phase)

	var machines vitiv1alpha1.MachineList
	if err := hit.client.Ctrl.List(context.Background(), &machines, ctrlclient.InNamespace(hit.cluster.Namespace)); err == nil {
		_, _ = fmt.Fprintf(out, "  machines          :\n")
		for i := range machines.Items {
			m := &machines.Items[i]
			if strings.HasPrefix(m.Name, clusterID+"-") {
				_, _ = fmt.Fprintf(out, "    %s  %s\n", m.Name, m.Status.Phase)
			}
		}
	}
	if kcDeleteSkipPreclean {
		_, _ = fmt.Fprintf(out, "\n⚠️  --skip-preclean: external state held by the guest (IPAM, DNS, volumes, ROR) will NOT be cleaned up.\n")
	}
	_, _ = fmt.Fprintf(out, "\nThis deletes the VMs and is IRREVERSIBLE.\n")
}

// confirmKcDelete requires the operator to type the cluster name back —
// a stronger guard than yes/no for an operation of this magnitude. Reads
// from cmd.InOrStdin so it is testable and scriptable.
func confirmKcDelete(cmd *cobra.Command, name string) error {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Type the cluster name to confirm decommission: ")
	r := bufio.NewReader(cmd.InOrStdin())
	answer, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	if strings.TrimSpace(answer) != name {
		return fmt.Errorf("aborted: confirmation did not match")
	}
	return nil
}

func init() {
	kcDeleteCmd.Flags().StringVarP(&kcDeleteAZ, "availabilityzone", "z", "", "restrict the search to a single availability zone")
	kcDeleteCmd.Flags().StringVarP(&kcDeleteNamespace, "namespace", "n", "", "namespace of the KubernetesCluster")
	kcDeleteCmd.Flags().BoolVar(&kcDeleteYes, "yes", false, "skip the confirmation prompt")
	kcDeleteCmd.Flags().BoolVar(&kcDeleteSkipPreclean, "skip-preclean", false,
		"skip phase 1 (guest cleanup) — external state held by the guest will NOT be cleaned up")
	kcDeleteCmd.Flags().DurationVar(&kcDeleteMachineTimeout, "machine-timeout", 15*time.Minute, "how long to wait for VM teardown")
	kcDeleteCmd.Flags().StringVar(&kcDeleteRORURL, "ror-url", "", "ROR API base URL (default https://api.ror.nhn.no)")
	kubernetesClusterCmd.AddCommand(kcDeleteCmd)
}
