package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/extract"
	"github.com/vitistack/vitictl/internal/health"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/login"
	"github.com/vitistack/vitictl/internal/talos"
)

var (
	kcHealthNamespace string
	kcHealthFull      bool
	kcHealthEndpoints []string
	kcHealthUseVIP    bool
	kcHealthIPv6      bool

	kcHealthAllNamespace string
	kcHealthAllOutput    string
)

var kcHealthCmd = &cobra.Command{
	Use:   "health [name]",
	Short: "Check the health of a KubernetesCluster",
	Long: `Reports the health of a single KubernetesCluster using its stored
credentials secret (no prior "viti kc login" required).

Without a name, an interactive picker (fzf) lets you choose a cluster — the
same selector "viti kc login" uses.

Default mode (lightweight) queries the cluster's API server health endpoints
through its kube.config:
  /readyz?verbose   — is the API server ready to serve
  /livez?verbose    — is the API server alive

--full mode runs a deep check:
  • Talos clusters → "talosctl health" (etcd quorum, members up, all nodes
    Ready, control-plane components healthy). Control-plane endpoints are
    resolved the same way "kc login"/"kc console" do and probed for
    reachability first; override with --endpoint.
  • other providers → verbose /healthz, /livez, /readyz plus a node
    readiness summary.

Exits non-zero when the cluster is unhealthy.`,
	Args: cobra.MaximumNArgs(1),
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

		var hit *kcHit
		if len(args) == 1 {
			hit, err = findClusterAcrossAZs(ctx, clients, args[0], kcHealthNamespace)
		} else {
			hit, err = fzfSelectCluster(collectClusters(ctx, clients, kcHealthNamespace))
		}
		if err != nil {
			return err
		}

		secret, err := extract.FindClusterSecret(ctx, hit.client.Ctrl, hit.cluster)
		if err != nil {
			return err
		}

		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "🩺 %s/%s (clusterId %s, AZ %s, provider %s)\n",
			hit.cluster.Namespace, hit.cluster.Name, hit.cluster.Spec.Cluster.ClusterId,
			hit.client.AZ.Name, hit.cluster.Spec.Cluster.Provider)

		if kcHealthFull && hit.cluster.Spec.Cluster.Provider == vitiv1alpha1.KubernetesProviderTypeTalos {
			return talosFullHealth(ctx, cmd, hit, secret)
		}
		return apiServerHealth(ctx, cmd, secret, kcHealthFull)
	},
}

// apiServerHealth runs the kube-API health-endpoint checks against the
// cluster's kube.config. In default mode it probes /readyz and /livez; in
// --full mode (non-Talos providers) it adds /healthz and a node-readiness
// summary. Returns a non-nil error when the cluster is not healthy so the
// command exits non-zero.
func apiServerHealth(ctx context.Context, cmd *cobra.Command, secret *corev1.Secret, full bool) error {
	out := cmd.OutOrStdout()
	kc := secret.Data[extract.KeyKubeConfig]
	if len(kc) == 0 {
		return fmt.Errorf("secret %s/%s has no %q entry — cannot reach the API server",
			secret.Namespace, secret.Name, extract.KeyKubeConfig)
	}
	cfg, err := health.RESTConfigFromKubeconfig(kc)
	if err != nil {
		return err
	}

	paths := []string{"/readyz", "/livez"}
	if full {
		paths = []string{"/healthz", "/livez", "/readyz"}
	}

	healthy := true
	for _, p := range paths {
		res := health.CheckEndpoint(ctx, cfg, p, true)
		switch {
		case res.Err != nil:
			healthy = false
			_, _ = fmt.Fprintf(out, "❌ %s — unreachable: %v\n", p, res.Err)
		case res.OK:
			_, _ = fmt.Fprintf(out, "✅ %s — ok\n", p)
		default:
			healthy = false
			failed := health.FailedChecks(res.Body)
			if len(failed) > 0 {
				_, _ = fmt.Fprintf(out, "❌ %s — failed checks: %s\n", p, strings.Join(failed, ", "))
			} else {
				_, _ = fmt.Fprintf(out, "❌ %s — not ok\n", p)
			}
		}
		if full {
			// In full mode show the verbose body for context.
			if body := strings.TrimSpace(res.Body); body != "" {
				_, _ = fmt.Fprintln(out, indent(body, "   "))
			}
		}
	}

	if full {
		summary, nerr := health.CheckNodes(ctx, cfg)
		if nerr != nil {
			healthy = false
			_, _ = fmt.Fprintf(out, "❌ nodes — %v\n", nerr)
		} else if summary.AllReady() {
			_, _ = fmt.Fprintf(out, "✅ nodes — %d/%d Ready\n", summary.Ready, summary.Total)
		} else {
			healthy = false
			_, _ = fmt.Fprintf(out, "❌ nodes — %d/%d Ready (not ready: %s)\n",
				summary.Ready, summary.Total, strings.Join(summary.NotReady, ", "))
		}
	}

	if !healthy {
		return fmt.Errorf("cluster is unhealthy")
	}
	_, _ = fmt.Fprintln(out, "🟢 cluster is healthy")
	return nil
}

// talosFullHealth resolves reachable control-plane endpoints and the
// cluster's node roles, then runs "talosctl health" against them using a
// temporary talosconfig generated from the credentials secret.
func talosFullHealth(ctx context.Context, cmd *cobra.Command, hit *kcHit, secret *corev1.Secret) error {
	if !talos.HasTalosctl() {
		return fmt.Errorf("talosctl not found on PATH — install it from https://www.talos.dev/latest/talos-guides/install/talosctl/ (or run without --full for the API-server health check)")
	}

	endpoints, err := resolveEtcdEndpoints(ctx, cmd, hit, kcHealthEndpoints, kcHealthUseVIP, kcHealthIPv6)
	if err != nil {
		return err
	}

	cpNodes, workerNodes, nodeWarn, nerr := login.ResolveNodeRoles(
		ctx, hit.client.Ctrl, hit.cluster.Namespace, hit.cluster.Spec.Cluster.ClusterId, kcHealthIPv6,
	)
	if nerr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "⚠️  %s\n", nerr)
	}
	if nodeWarn != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "⚠️  %s\n", nodeWarn)
	}

	cfgPath, cleanup, err := talos.WriteTempTalosconfig(secret, hit.cluster.Spec.Cluster.ClusterId, endpoints)
	if err != nil {
		return err
	}
	defer cleanup()

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"🩺 talosctl health — endpoints: %v, control-plane: %v, workers: %v\n",
		endpoints, cpNodes, workerNodes)
	return talos.Health(cfgPath, endpoints, cpNodes, workerNodes)
}

// indent prefixes every line of s with prefix.
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// --- health-all -----------------------------------------------------------

// healthRow is one row of the health-all report.
type healthRow struct {
	AvailabilityZone string `json:"availabilityZone"`
	Cluster          string `json:"cluster"`
	ClusterID        string `json:"clusterId"`
	Namespace        string `json:"namespace"`
	OK               bool   `json:"ok"`
	Message          string `json:"message,omitempty"`
}

var kcHealthAllCmd = &cobra.Command{
	Use:     "health-all",
	Aliases: []string{"healthall"},
	Short:   "Check API-server readiness for every KubernetesCluster",
	Long: `Probes /readyz on every KubernetesCluster across the configured
availability zones (using each cluster's own kube.config) and prints a
table of cluster name, clusterId, namespace, status, and any message.

Status is true only when the cluster's API server returns a healthy
/readyz. Checks run concurrently. Exits non-zero if any cluster is
unhealthy or unreachable.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := context.Background()
		zones, err := kube.ResolveAvailabilityZones(AvailabilityZone())
		if err != nil {
			return err
		}
		clients, err := kube.ConnectAll(ctx, zones, true, warn)
		if err != nil {
			return err
		}
		hits := collectClusters(ctx, clients, kcHealthAllNamespace)
		if len(hits) == 0 {
			fmt.Println("🤷 no kubernetesclusters found")
			return nil
		}

		rows := make([]healthRow, len(hits))
		var wg sync.WaitGroup
		for i := range hits {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				rows[i] = checkClusterReadyz(ctx, hits[i])
			}(i)
		}
		wg.Wait()

		sort.Slice(rows, func(a, b int) bool {
			if rows[a].AvailabilityZone != rows[b].AvailabilityZone {
				return rows[a].AvailabilityZone < rows[b].AvailabilityZone
			}
			if rows[a].Namespace != rows[b].Namespace {
				return rows[a].Namespace < rows[b].Namespace
			}
			return rows[a].Cluster < rows[b].Cluster
		})

		if err := renderHealthRows(cmd, rows, kcHealthAllOutput); err != nil {
			return err
		}
		for _, r := range rows {
			if !r.OK {
				return fmt.Errorf("%d/%d cluster(s) unhealthy", countUnhealthy(rows), len(rows))
			}
		}
		return nil
	},
}

// checkClusterReadyz resolves a cluster's secret + kube.config and probes
// /readyz, returning a populated healthRow (never panics; all failures are
// captured as OK=false + Message).
func checkClusterReadyz(ctx context.Context, hit kcHit) healthRow {
	row := healthRow{
		AvailabilityZone: hit.client.AZ.Name,
		Cluster:          hit.cluster.Name,
		ClusterID:        hit.cluster.Spec.Cluster.ClusterId,
		Namespace:        hit.cluster.Namespace,
	}

	secret, err := extract.FindClusterSecret(ctx, hit.client.Ctrl, hit.cluster)
	if err != nil {
		row.Message = "secret: " + health.FirstLine(err.Error())
		return row
	}
	kc := secret.Data[extract.KeyKubeConfig]
	if len(kc) == 0 {
		row.Message = "secret has no " + extract.KeyKubeConfig
		return row
	}
	cfg, err := health.RESTConfigFromKubeconfig(kc)
	if err != nil {
		row.Message = health.FirstLine(err.Error())
		return row
	}

	res := health.CheckEndpoint(ctx, cfg, "/readyz", true)
	switch {
	case res.Err != nil:
		row.Message = "unreachable: " + health.FirstLine(res.Err.Error())
	case res.OK:
		row.OK = true
	default:
		if failed := health.FailedChecks(res.Body); len(failed) > 0 {
			row.Message = "readyz failed: " + strings.Join(failed, ", ")
		} else {
			row.Message = "readyz not ok"
		}
	}
	return row
}

func countUnhealthy(rows []healthRow) int {
	n := 0
	for _, r := range rows {
		if !r.OK {
			n++
		}
	}
	return n
}

func renderHealthRows(cmd *cobra.Command, rows []healthRow, output string) error {
	switch strings.ToLower(output) {
	case "json":
		data, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	case "yaml", "yml":
		data, err := yaml.Marshal(rows)
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(data)
		return err
	case "", "table":
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NAMESPACE\tNAME\tCLUSTER ID\tAZ\tSTATUS\tMESSAGE")
		for _, r := range rows {
			status := "❌"
			if r.OK {
				status = "✅"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				r.Namespace, r.Cluster, valueOrDash(r.ClusterID), r.AvailabilityZone, status, valueOrDash(r.Message))
		}
		return tw.Flush()
	default:
		return fmt.Errorf("unsupported output format %q (valid: table, json, yaml)", output)
	}
}

func init() {
	kcHealthCmd.Flags().StringVarP(&kcHealthNamespace, "namespace", "n", "", "namespace of the KubernetesCluster")
	kcHealthCmd.Flags().BoolVar(&kcHealthFull, "full", false,
		"deep health check: talosctl health (Talos) or verbose healthz/livez/readyz + node readiness (other providers)")
	kcHealthCmd.Flags().StringArrayVar(&kcHealthEndpoints, "endpoint", nil,
		"explicit Talos API endpoint for --full on Talos clusters (repeatable); default: auto-resolved CP endpoints")
	kcHealthCmd.Flags().BoolVar(&kcHealthUseVIP, "use-vip", false,
		"include the CPVIP load-balancer address(es) in the resolved Talos endpoints (--full, Talos only)")
	kcHealthCmd.Flags().BoolVar(&kcHealthIPv6, "ipv6", false,
		"include IPv6 control-plane endpoints/nodes (--full, Talos only; default: IPv4 only)")

	kcHealthAllCmd.Flags().StringVarP(&kcHealthAllNamespace, "namespace", "n", "", "limit to this namespace")
	kcHealthAllCmd.Flags().StringVarP(&kcHealthAllOutput, "output", "o", "", "output format: table, json, yaml (default: table)")

	kubernetesClusterCmd.AddCommand(kcHealthCmd, kcHealthAllCmd)
}
