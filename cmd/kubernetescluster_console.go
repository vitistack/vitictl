package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/vitistack/vitictl/internal/console"
	"github.com/vitistack/vitictl/internal/extract"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/login"
)

var (
	kcConsoleNamespace string
	kcConsoleEndpoints []string
	kcConsoleUseVIP    bool
)

var kcConsoleCmd = &cobra.Command{
	Use:   "console <name>",
	Short: "Open the provider-native dashboard for a KubernetesCluster",
	Long: `For Talos clusters this launches "talosctl dashboard" against the
control-plane nodes, using a temporary talosconfig generated from the
cluster's credentials secret (no prior "viti kc login" required).

Other providers print provider-specific guidance.

Control-plane endpoints are resolved the same way "viti kc login" does
(CPVIP pool members → Machine objects named <clusterId>-ctp* → fallback),
overridable with --endpoint (repeatable).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		zones, err := kube.ResolveAvailabilityZones(AvailabilityZone())
		if err != nil {
			return err
		}
		clients, err := kube.ConnectAll(ctx, zones, true, warn)
		if err != nil {
			return err
		}
		hit, err := findClusterAcrossAZs(ctx, clients, args[0], kcConsoleNamespace)
		if err != nil {
			return err
		}
		secret, err := extract.FindClusterSecret(ctx, hit.client.Ctrl, hit.cluster)
		if err != nil {
			return err
		}

		handler, err := console.ForProvider(hit.cluster.Spec.Cluster.Provider)
		if err != nil {
			return err
		}

		endpoints := kcConsoleEndpoints
		if len(endpoints) == 0 {
			resolved, src, warnings, rerr := login.ResolveControlPlaneEndpoints(
				ctx, hit.client.Ctrl, hit.cluster.Namespace, hit.cluster.Spec.Cluster.ClusterId, kcConsoleUseVIP,
			)
			if rerr != nil {
				return rerr
			}
			for _, w := range warnings {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "⚠️  %s\n", w)
			}
			if len(resolved) > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "ℹ️  using endpoints from %s\n", src)
			}
			endpoints = resolved
		}

		return handler.ClusterConsole(ctx, console.ClusterRequest{
			Cluster:   hit.cluster,
			Secret:    secret,
			Client:    hit.client,
			Endpoints: endpoints,
		})
	},
}

func init() {
	kcConsoleCmd.Flags().StringVarP(&kcConsoleNamespace, "namespace", "n", "", "namespace of the KubernetesCluster")
	kcConsoleCmd.Flags().StringArrayVar(&kcConsoleEndpoints, "endpoint", nil,
		"explicit control-plane endpoint (repeatable); overrides auto-resolution")
	kcConsoleCmd.Flags().BoolVar(&kcConsoleUseVIP, "use-vip", false,
		"include the CPVIP load-balancer address(es) in the endpoint list (default: control-plane node IPs only)")

	kubernetesClusterCmd.AddCommand(kcConsoleCmd)
}
