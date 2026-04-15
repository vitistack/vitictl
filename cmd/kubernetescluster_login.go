package cmd

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/extract"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/login"
)

var (
	kcLoginNamespace    string
	kcLoginOutputDir    string
	kcLoginEndpoints    []string
	kcLoginEndpointFrom string // "auto" (default) | "secret"
	kcLoginContextName  string
	kcLoginForce        bool
	kcLoginNoActivate   bool
	kcLoginUseVIP       bool
)

var kcLoginCmd = &cobra.Command{
	Use:   "login <name>",
	Short: "Merge a KubernetesCluster's kubeconfig (and talosconfig) into your local tooling",
	Long: `Fetches the KubernetesCluster's credentials secret and installs the
kubeconfig + talosconfig as a new context on this machine.

By default the context name is the cluster's clusterId. Existing contexts
with the same name are refused — pass --force to overwrite.

For Talos clusters the talosconfig's endpoints are rewritten to the
cluster's control-plane addresses, discovered in this order:
  1. ControlPlaneVirtualSharedIP (CPVIP) pool members for this cluster
  2. Machine objects named <clusterId>-ctp* (their status.ipAddresses)
  3. fall back to whatever the secret's talosconfig already contains
Pass --endpoint-from secret to always keep what's in the secret, or
--endpoint <addr> (repeatable) to set specific endpoints.

Use -o/--output-dir <path> to write "kubeconfig-<clusterId>" and
"talosconfig-<clusterId>" files into <path> instead of merging into the
default kubectl/talosctl config files.

Requires kubectl and/or talosctl to be installed; missing tools cause the
matching merge step to be skipped with a warning.`,
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
		hit, err := findClusterAcrossAZs(ctx, clients, args[0], kcLoginNamespace)
		if err != nil {
			return err
		}
		secret, err := extract.FindClusterSecret(ctx, hit.client.Ctrl, hit.cluster)
		if err != nil {
			return err
		}

		contextName := kcLoginContextName
		if contextName == "" {
			contextName = hit.cluster.Spec.Cluster.ClusterId
		}
		if contextName == "" {
			return fmt.Errorf("cluster %s/%s has no spec.data.clusterId and no --context-name override was given",
				hit.cluster.Namespace, hit.cluster.Name)
		}

		kubectlOK, talosctlOK := checkLoginCLIs(cmd, hit.cluster.Spec.Cluster.Provider)
		if !kubectlOK && !talosctlOK {
			return fmt.Errorf("neither kubectl nor talosctl is on PATH — nothing to configure")
		}

		// --- kubeconfig -----------------------------------------------------
		if kubectlOK {
			if err := doKubeconfig(cmd, secret, contextName); err != nil {
				return err
			}
		}

		// --- talosconfig (Talos clusters only) ------------------------------
		if hit.cluster.Spec.Cluster.Provider == vitiv1alpha1.KubernetesProviderTypeTalos {
			if talosctlOK {
				if err := doTalosconfig(cmd, ctx, hit, secret, contextName); err != nil {
					return err
				}
			}
		}
		return nil
	},
}

// checkLoginCLIs reports which of kubectl / talosctl are available.
// Missing tools are warned about — but only for the cases that matter for
// the current cluster type (non-Talos clusters don't need talosctl).
func checkLoginCLIs(cmd *cobra.Command, provider vitiv1alpha1.KubernetesProviderType) (kubectlOK, talosctlOK bool) {
	if _, err := exec.LookPath("kubectl"); err == nil {
		kubectlOK = true
	} else {
		_, _ = fmt.Fprintln(cmd.OutOrStderr(), "⚠️  kubectl not found on PATH — skipping kubeconfig merge")
	}
	needsTalos := provider == vitiv1alpha1.KubernetesProviderTypeTalos
	if _, err := exec.LookPath("talosctl"); err == nil {
		talosctlOK = true
	} else if needsTalos {
		_, _ = fmt.Fprintln(cmd.OutOrStderr(), "⚠️  talosctl not found on PATH — skipping talosconfig merge")
	}
	return kubectlOK, talosctlOK
}

func doKubeconfig(cmd *cobra.Command, secret *corev1.Secret, contextName string) error {
	kc := secret.Data[extract.KeyKubeConfig]
	if len(kc) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStderr(), "⚠️  secret has no kube.config entry — skipping kubeconfig step")
		return nil
	}
	if kcLoginOutputDir != "" {
		path := filepath.Join(kcLoginOutputDir, "kubeconfig-"+contextName)
		if err := login.WriteKubeconfigFile(kc, contextName, path); err != nil {
			return fmt.Errorf("writing kubeconfig file: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "📄 wrote %s (context %q)\n", path, contextName)
		return nil
	}
	out, err := login.MergeKubeconfig(kc, contextName, "", !kcLoginNoActivate, kcLoginForce)
	if err != nil {
		return fmt.Errorf("merging kubeconfig: %w", err)
	}
	msg := "merged"
	if out.Overwrote {
		msg = "overwrote"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "🔑 kubeconfig: %s context %q in %s\n", msg, out.Context, out.Path)
	if out.Activated {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "   current-context set to %q\n", out.Context)
	}
	return nil
}

func doTalosconfig(cmd *cobra.Command, ctx context.Context, hit *kcHit, secret *corev1.Secret, contextName string) error {
	tc := secret.Data[extract.KeyTalosconfig]
	if len(tc) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStderr(), "⚠️  secret has no talosconfig entry — skipping talosconfig step")
		return nil
	}

	// Resolve endpoints.
	var endpoints []string
	var source login.EndpointSource
	switch {
	case len(kcLoginEndpoints) > 0:
		endpoints = kcLoginEndpoints
		source = login.SourceOverride
	case kcLoginEndpointFrom == "secret":
		// Leave endpoints nil so the secret's own values survive the merge.
		source = login.SourceSecret
	default:
		resolved, src, warnings, err := login.ResolveControlPlaneEndpoints(
			ctx, hit.client.Ctrl, hit.cluster.Namespace, hit.cluster.Spec.Cluster.ClusterId, kcLoginUseVIP,
		)
		if err != nil {
			return fmt.Errorf("resolving control-plane endpoints: %w", err)
		}
		for _, w := range warnings {
			_, _ = fmt.Fprintf(cmd.OutOrStderr(), "⚠️  %s\n", w)
		}
		if len(resolved) == 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStderr(), "⚠️  no control-plane addresses found in CPVIP or Machines — keeping the secret's endpoints")
			source = login.SourceSecret
		} else {
			endpoints = resolved
			source = src
		}
	}

	if kcLoginOutputDir != "" {
		path := filepath.Join(kcLoginOutputDir, "talosconfig-"+contextName)
		if err := login.WriteTalosconfigFile(tc, contextName, endpoints, path); err != nil {
			return fmt.Errorf("writing talosconfig file: %w", err)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "📄 wrote %s (context %q", path, contextName)
		if len(endpoints) > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), ", endpoints from %s: %v", source, endpoints)
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), ")")
		return nil
	}

	out, err := login.MergeTalosconfig(tc, contextName, endpoints, "", !kcLoginNoActivate, kcLoginForce)
	if err != nil {
		return fmt.Errorf("merging talosconfig: %w", err)
	}
	msg := "merged"
	if out.Overwrote {
		msg = "overwrote"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "🛠️  talosconfig: %s context %q in %s\n", msg, out.Context, out.Path)
	if len(out.Endpoints) > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "   endpoints (%s): %v\n", source, out.Endpoints)
	}
	if out.Activated {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "   current-context set to %q\n", out.Context)
	}
	return nil
}

func init() {
	kcLoginCmd.Flags().StringVarP(&kcLoginNamespace, "namespace", "n", "", "namespace of the KubernetesCluster")
	kcLoginCmd.Flags().StringVarP(&kcLoginOutputDir, "output-dir", "o", "",
		"write kubeconfig-<clusterId> / talosconfig-<clusterId> files into this directory instead of merging")
	kcLoginCmd.Flags().StringArrayVar(&kcLoginEndpoints, "endpoint", nil,
		"explicit talos endpoint (repeatable); overrides auto-resolution")
	kcLoginCmd.Flags().StringVar(&kcLoginEndpointFrom, "endpoint-from", "",
		"set to \"secret\" to keep the talosconfig's original endpoints (default: auto-resolve from CPVIP / Machines)")
	kcLoginCmd.Flags().StringVar(&kcLoginContextName, "context-name", "", "override the context name (default: clusterId)")
	kcLoginCmd.Flags().BoolVar(&kcLoginForce, "force", false, "overwrite an existing context with the same name")
	kcLoginCmd.Flags().BoolVar(&kcLoginNoActivate, "no-activate", false, "merge but do not change current-context")
	kcLoginCmd.Flags().BoolVar(&kcLoginUseVIP, "use-vip", false,
		"include the CPVIP load-balancer address(es) in the resolved talos endpoints (default: control-plane node IPs only)")

	kubernetesClusterCmd.AddCommand(kcLoginCmd)
}
