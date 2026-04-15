package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"

	etcdpkg "github.com/vitistack/vitictl/internal/etcd"
	"github.com/vitistack/vitictl/internal/extract"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/login"
	"github.com/vitistack/vitictl/internal/talos"
)

var (
	kcEtcdBackupNamespace string
	kcEtcdBackupOutput    string
	kcEtcdBackupNode      string
	kcEtcdBackupEndpoints []string
	kcEtcdBackupCopyRaw   bool

	kcEtcdRestoreNamespace     string
	kcEtcdRestoreFrom          string
	kcEtcdRestoreNode          string
	kcEtcdRestoreEndpoints     []string
	kcEtcdRestoreYes           bool
	kcEtcdRestoreSkipHashCheck bool
)

var kcEtcdBackupCmd = &cobra.Command{
	Use:     "etcd-backup <name>",
	Aliases: []string{"etcdbackup", "snapshot"},
	Short:   "Take an etcd snapshot of a KubernetesCluster",
	Long: `For Talos clusters this runs "talosctl etcd snapshot" against one
control-plane node and downloads the binary snapshot file locally.

The control-plane node is auto-picked from the CPVIP pool members
(--node <addr> overrides). Endpoints come from the same CPVIP / ctp-machine
resolver "kc console" / "kc login" use; --endpoint <addr> overrides.

The output path passed via -o/--output is interpreted as:
  - a directory (existing, or any path ending in "/") → the file is
    written as "<dir>/etcd-backup-<clusterId>.snapshot"
  - anything else → used verbatim as the output file path
Missing parent directories are created automatically.

Pass --copy-raw to use the unhealthy-cluster fallback documented by Talos
(copies /var/lib/etcd/member/snap/db). Such snapshots lack the integrity
hash and must be restored with --skip-hash-check.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if kcEtcdBackupOutput == "" {
			return fmt.Errorf("-o/--output is required (target file path or directory)")
		}
		ctx := context.Background()
		hit, secret, err := resolveClusterAndSecret(ctx, args[0], kcEtcdBackupNamespace)
		if err != nil {
			return err
		}
		handler, err := etcdpkg.ForProvider(hit.cluster.Spec.Cluster.Provider)
		if err != nil {
			return err
		}
		endpoints, err := resolveEtcdEndpoints(ctx, cmd, hit, kcEtcdBackupEndpoints)
		if err != nil {
			return err
		}
		node := kcEtcdBackupNode
		if node == "" {
			node = endpoints[0]
		}
		outputPath, err := resolveSnapshotOutputPath(kcEtcdBackupOutput, hit.cluster.Spec.Cluster.ClusterId)
		if err != nil {
			return err
		}

		return handler.Snapshot(ctx, etcdpkg.SnapshotRequest{
			Cluster:   hit.cluster,
			Secret:    secret,
			Client:    hit.client,
			Endpoints: endpoints,
			Node:      node,
			Output:    outputPath,
			CopyRaw:   kcEtcdBackupCopyRaw,
		})
	},
}

var kcEtcdRestoreCmd = &cobra.Command{
	Use:     "etcd-restore <name>",
	Aliases: []string{"etcdrestore", "recover"},
	Short:   "Restore etcd from a snapshot file (DESTRUCTIVE)",
	Long: `Restores etcd from a previously-taken snapshot using
"talosctl bootstrap --recover-from <file>" against one control-plane node.

⚠️  THIS IS DESTRUCTIVE. Per Talos disaster-recovery docs:
  - All etcd service instances on the cluster's CP nodes must be in the
    Preparing state (etcd not running) before invoking this.
  - No CP node may have machine type "init"; convert with
    "talosctl edit mc --mode=staged" first if needed.
  - For hardware loss, prepare fresh CP nodes before restoring.

Pass --yes to skip the confirmation prompt. Pass --skip-hash-check when
the snapshot was produced with "viti kc etcd-backup --copy-raw" (raw
file copy, no integrity hash).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if kcEtcdRestoreFrom == "" {
			return fmt.Errorf("--from <snapshot-file> is required")
		}
		if _, err := os.Stat(kcEtcdRestoreFrom); err != nil {
			return fmt.Errorf("snapshot file %s: %w", kcEtcdRestoreFrom, err)
		}

		ctx := context.Background()
		hit, secret, err := resolveClusterAndSecret(ctx, args[0], kcEtcdRestoreNamespace)
		if err != nil {
			return err
		}
		handler, err := etcdpkg.ForProvider(hit.cluster.Spec.Cluster.Provider)
		if err != nil {
			return err
		}
		endpoints, err := resolveEtcdEndpoints(ctx, cmd, hit, kcEtcdRestoreEndpoints)
		if err != nil {
			return err
		}
		node := kcEtcdRestoreNode
		if node == "" {
			node = endpoints[0]
		}

		if !kcEtcdRestoreYes {
			if err := confirmDestructive(cmd, hit.cluster.Spec.Cluster.ClusterId, node, kcEtcdRestoreFrom); err != nil {
				return err
			}
		}

		return handler.Restore(ctx, etcdpkg.RestoreRequest{
			Cluster:       hit.cluster,
			Secret:        secret,
			Client:        hit.client,
			Endpoints:     endpoints,
			Node:          node,
			Input:         kcEtcdRestoreFrom,
			SkipHashCheck: kcEtcdRestoreSkipHashCheck,
		})
	},
}

// resolveClusterAndSecret is shared by etcd-backup and etcd-restore. It
// finds the cluster across configured AZs and pulls its credentials
// secret. Errors propagate verbatim.
func resolveClusterAndSecret(ctx context.Context, name, namespace string) (*kcHit, *corev1.Secret, error) {
	zones, err := kube.ResolveAvailabilityZones(AvailabilityZone())
	if err != nil {
		return nil, nil, err
	}
	clients, err := kube.ConnectAll(ctx, zones, true, warn)
	if err != nil {
		return nil, nil, err
	}
	hit, err := findClusterAcrossAZs(ctx, clients, name, namespace)
	if err != nil {
		return nil, nil, err
	}
	secret, err := extract.FindClusterSecret(ctx, hit.client.Ctrl, hit.cluster)
	if err != nil {
		return nil, nil, err
	}
	return hit, secret, nil
}

// resolveEtcdEndpoints returns the user-overridden --endpoint list when
// non-empty; otherwise it auto-resolves CP endpoints via the CPVIP /
// ctp-machine resolver. In both cases a pre-flight TCP probe on the
// Talos API port filters down to addresses that are actually reachable
// from this machine, surfacing unreachable addresses as warnings and
// erroring if none respond. This avoids the "i/o timeout deep inside
// talosctl" failure mode when CPs are on a split-horizon network.
func resolveEtcdEndpoints(ctx context.Context, cmd *cobra.Command, hit *kcHit, override []string) ([]string, error) {
	var candidates []string
	var source string
	if len(override) > 0 {
		candidates = override
		source = "--endpoint"
	} else {
		resolved, src, warnings, err := login.ResolveControlPlaneEndpoints(
			ctx, hit.client.Ctrl, hit.cluster.Namespace, hit.cluster.Spec.Cluster.ClusterId,
		)
		if err != nil {
			return nil, err
		}
		for _, w := range warnings {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "⚠️  %s\n", w)
		}
		if len(resolved) == 0 {
			return nil, fmt.Errorf("could not resolve any control-plane endpoints; pass --endpoint <addr>")
		}
		candidates = resolved
		source = string(src)
	}

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "ℹ️  endpoints from %s: %v\n", source, candidates)
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "🔎 probing tcp/%d reachability…\n", talos.APIPort)
	results, reachable := talos.ProbeReachable(candidates, talos.APIPort, talos.DefaultProbeTimeout)
	for _, r := range results {
		if r.OK {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "   ✅ %s:%d\n", r.Address, talos.APIPort)
		} else {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "   ❌ %s:%d (%v)\n", r.Address, talos.APIPort, r.Err)
		}
	}
	if len(reachable) == 0 {
		return nil, fmt.Errorf("none of the candidate endpoints are reachable on tcp/%d: %v — pass --endpoint <reachable-addr>", talos.APIPort, candidates)
	}
	return reachable, nil
}

// resolveSnapshotOutputPath turns the user's -o value into a concrete
// file path. It treats `out` as a directory if it either already exists
// as one or ends with a path separator (the user's hint that they want
// a dir, even if it doesn't exist yet). In both cases the default
// filename "etcd-backup-<clusterId>.snapshot" is appended. Otherwise
// `out` is treated verbatim as the output file path. Parent directories
// are created as needed in all cases.
func resolveSnapshotOutputPath(out, clusterID string) (string, error) {
	defaultName := "etcd-backup-" + clusterID + ".snapshot"

	asDir := strings.HasSuffix(out, "/") || strings.HasSuffix(out, string(os.PathSeparator))
	if !asDir {
		if info, err := os.Stat(out); err == nil && info.IsDir() {
			asDir = true
		}
	}
	if asDir {
		if err := os.MkdirAll(out, 0o700); err != nil {
			return "", fmt.Errorf("creating output dir: %w", err)
		}
		return filepath.Join(out, defaultName), nil
	}
	if dir := filepath.Dir(out); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("creating output dir: %w", err)
		}
	}
	return out, nil
}

// confirmDestructive prompts the user to type "yes" to proceed with a
// restore. Reads from cmd.InOrStdin so this is testable + scriptable
// (echo yes | …) but defaults to terminal stdin in normal use.
func confirmDestructive(cmd *cobra.Command, clusterID, node, snapshot string) error {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"\n⚠️  About to restore etcd on cluster %q via node %s from %s\n"+
			"   This is destructive. Type 'yes' to continue: ",
		clusterID, node, snapshot)
	r := bufio.NewReader(cmd.InOrStdin())
	answer, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	if strings.TrimSpace(answer) != "yes" {
		return fmt.Errorf("aborted")
	}
	return nil
}

func init() {
	kcEtcdBackupCmd.Flags().StringVarP(&kcEtcdBackupNamespace, "namespace", "n", "", "namespace of the KubernetesCluster")
	kcEtcdBackupCmd.Flags().StringVarP(&kcEtcdBackupOutput, "output", "o", "",
		"output file path or directory (required). Directory → etcd-backup-<clusterId>.snapshot inside it")
	kcEtcdBackupCmd.Flags().StringVar(&kcEtcdBackupNode, "node", "", "control-plane node to snapshot from (default: first endpoint)")
	kcEtcdBackupCmd.Flags().StringArrayVar(&kcEtcdBackupEndpoints, "endpoint", nil,
		"explicit Talos API endpoint (repeatable); default: auto-resolved CP endpoints")
	kcEtcdBackupCmd.Flags().BoolVar(&kcEtcdBackupCopyRaw, "copy-raw", false,
		"unhealthy-cluster fallback: cp /var/lib/etcd/member/snap/db (no integrity hash)")

	kcEtcdRestoreCmd.Flags().StringVarP(&kcEtcdRestoreNamespace, "namespace", "n", "", "namespace of the KubernetesCluster")
	kcEtcdRestoreCmd.Flags().StringVar(&kcEtcdRestoreFrom, "from", "", "snapshot file to restore from (required)")
	kcEtcdRestoreCmd.Flags().StringVar(&kcEtcdRestoreNode, "node", "", "control-plane node to bootstrap-restore against (default: first endpoint)")
	kcEtcdRestoreCmd.Flags().StringArrayVar(&kcEtcdRestoreEndpoints, "endpoint", nil,
		"explicit Talos API endpoint (repeatable); default: auto-resolved CP endpoints")
	kcEtcdRestoreCmd.Flags().BoolVar(&kcEtcdRestoreYes, "yes", false, "skip the destructive-action confirmation prompt")
	kcEtcdRestoreCmd.Flags().BoolVar(&kcEtcdRestoreSkipHashCheck, "skip-hash-check", false,
		"pass --recover-skip-hash-check to talosctl (required for snapshots taken with --copy-raw)")

	kubernetesClusterCmd.AddCommand(kcEtcdBackupCmd, kcEtcdRestoreCmd)
}
