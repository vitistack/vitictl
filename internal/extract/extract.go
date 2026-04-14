package extract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

const (
	ClusterIDLabel = "vitistack.io/clusterid"

	KeyTalosconfig    = "talosconfig"
	KeyControlplane   = "controlplane.yaml"
	KeyWorker         = "worker.yaml"
	KeySecretsBundle  = "secrets.bundle"
	KeyKubeConfig     = "kube.config"
	FileSecretYAML    = "secret.yaml"
	FileKubeconfigOut = "kubeconfig"
	FileInfoTxt       = "info.txt"
)

// WriteSummary is what the extract commands print to stdout.
type WriteSummary struct {
	OutputDir string
	Files     []string
}

// FindClusterSecret locates the Kubernetes Secret holding a cluster's
// config artifacts. It first tries a Secret named exactly <clusterId> in the
// KubernetesCluster's namespace, then falls back to a label-selector search
// on vitistack.io/clusterid=<clusterId> (which covers the Talos case where a
// SECRET_PREFIX may have been configured on the operator).
func FindClusterSecret(
	ctx context.Context,
	c ctrlclient.Client,
	cluster *vitiv1alpha1.KubernetesCluster,
) (*corev1.Secret, error) {
	clusterID := cluster.Spec.Cluster.ClusterId
	if clusterID == "" {
		return nil, fmt.Errorf("cluster %s/%s has no spec.data.clusterId", cluster.Namespace, cluster.Name)
	}

	var direct corev1.Secret
	err := c.Get(ctx, ctrlclient.ObjectKey{Namespace: cluster.Namespace, Name: clusterID}, &direct)
	if err == nil {
		return &direct, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading secret %s/%s: %w", cluster.Namespace, clusterID, err)
	}

	// Fallback: label selector.
	var list corev1.SecretList
	sel := labels.SelectorFromSet(labels.Set{ClusterIDLabel: clusterID})
	if err := c.List(ctx, &list, ctrlclient.InNamespace(cluster.Namespace), ctrlclient.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, fmt.Errorf("listing secrets by label: %w", err)
	}
	switch len(list.Items) {
	case 0:
		return nil, fmt.Errorf("no secret found for clusterId %s in namespace %s (tried name=%s and label %s=%s)",
			clusterID, cluster.Namespace, clusterID, ClusterIDLabel, clusterID)
	case 1:
		return &list.Items[0], nil
	default:
		names := make([]string, 0, len(list.Items))
		for _, s := range list.Items {
			names = append(names, s.Name)
		}
		return nil, fmt.Errorf("multiple secrets match clusterId %s in namespace %s: %s", clusterID, cluster.Namespace, strings.Join(names, ", "))
	}
}

// WriteTalos extracts talosconfig, controlplane.yaml, worker.yaml,
// secret.yaml (from secrets.bundle), kubeconfig (from kube.config), and an
// info.txt file with all remaining keys.
func WriteTalos(outputDir string, secret *corev1.Secret) (*WriteSummary, error) {
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating output dir: %w", err)
	}
	// name in secret -> output filename
	knownFiles := map[string]string{
		KeyTalosconfig:   KeyTalosconfig,
		KeyControlplane:  KeyControlplane,
		KeyWorker:        KeyWorker,
		KeySecretsBundle: FileSecretYAML,
		KeyKubeConfig:    FileKubeconfigOut,
	}
	written := make([]string, 0, len(knownFiles)+1)

	for key, filename := range knownFiles {
		data, ok := secret.Data[key]
		if !ok || len(data) == 0 {
			continue
		}
		fp := filepath.Join(outputDir, filename)
		if err := os.WriteFile(fp, data, 0o600); err != nil {
			return nil, fmt.Errorf("writing %s: %w", fp, err)
		}
		written = append(written, fp)
	}

	info, extraKeys := buildInfoTxt(secret, knownFiles)
	if len(extraKeys) > 0 {
		infoPath := filepath.Join(outputDir, FileInfoTxt)
		if err := os.WriteFile(infoPath, []byte(info), 0o600); err != nil {
			return nil, fmt.Errorf("writing %s: %w", infoPath, err)
		}
		written = append(written, infoPath)
	}

	sort.Strings(written)
	return &WriteSummary{OutputDir: outputDir, Files: written}, nil
}

// WriteAKS extracts the AKS kubeconfig (kube.config) and an info.txt file
// with the rest of the secret contents (AKS metadata, flags, etc.).
func WriteAKS(outputDir string, secret *corev1.Secret) (*WriteSummary, error) {
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating output dir: %w", err)
	}
	written := make([]string, 0, 2)

	if data, ok := secret.Data[KeyKubeConfig]; ok && len(data) > 0 {
		fp := filepath.Join(outputDir, FileKubeconfigOut)
		if err := os.WriteFile(fp, data, 0o600); err != nil {
			return nil, fmt.Errorf("writing %s: %w", fp, err)
		}
		written = append(written, fp)
	}

	knownFiles := map[string]string{KeyKubeConfig: FileKubeconfigOut}
	info, extraKeys := buildInfoTxt(secret, knownFiles)
	if len(extraKeys) > 0 {
		infoPath := filepath.Join(outputDir, FileInfoTxt)
		if err := os.WriteFile(infoPath, []byte(info), 0o600); err != nil {
			return nil, fmt.Errorf("writing %s: %w", infoPath, err)
		}
		written = append(written, infoPath)
	}

	sort.Strings(written)
	return &WriteSummary{OutputDir: outputDir, Files: written}, nil
}

func buildInfoTxt(secret *corev1.Secret, skipKeys map[string]string) (string, []string) {
	keys := make([]string, 0, len(secret.Data))
	for k := range secret.Data {
		if _, skip := skipKeys[k]; skip {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "", keys
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Secret: %s/%s\n\n", secret.Namespace, secret.Name)
	for _, k := range keys {
		fmt.Fprintf(&sb, "## %s\n%s\n\n", k, string(secret.Data[k]))
	}
	return sb.String(), keys
}
