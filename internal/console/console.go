// Package console dispatches provider-specific "show me the dashboard for
// this thing" behaviour. It is provider-agnostic: each supported provider
// registers a Handler in its own file and the cobra subcommands look one
// up by the cluster's (or machine's) provider type.
package console

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/kube"
)

// ClusterRequest is what `viti kc console` hands to a provider handler
// after it has resolved the cluster, fetched the credentials secret, and
// (optionally) resolved control-plane endpoints.
//
// Endpoints and Nodes are kept separate, mirroring talosctl semantics: for
// Talos, --endpoints lists the API addresses the client connects to (control
// planes only — that's where the API runs) while --nodes targets the hosts
// the dashboard should display data for (control planes AND workers, so the
// whole cluster is visible).
type ClusterRequest struct {
	Cluster   *vitiv1alpha1.KubernetesCluster
	Secret    *corev1.Secret
	Client    *kube.Client
	Endpoints []string // API addresses (control planes)
	Nodes     []string // dashboard target nodes (all machines)
}

// MachineRequest is what `viti machine console` hands to a handler. The
// owning cluster and its secret are included because a machine alone
// usually doesn't have the credentials needed to talk to it (for Talos,
// the PKI lives on the cluster's secret).
//
// Endpoints and Nodes are kept separate: for Talos, --endpoints lists the
// API addresses the client connects to (typically the cluster's control
// planes, cert-valid) while --nodes specifies which node(s) the commands
// target (the machine we actually want a dashboard for).
type MachineRequest struct {
	Machine   *vitiv1alpha1.Machine
	Cluster   *vitiv1alpha1.KubernetesCluster // owning cluster (may be nil if not found)
	Secret    *corev1.Secret                  // cluster credentials secret
	Client    *kube.Client
	Endpoints []string // API endpoints (cluster-wide, cert-valid)
	Nodes     []string // target node addresses for this machine
}

// Handler dispatches a cluster- or machine-scoped console request.
// Providers that don't support one of the two should return a clear
// "not supported" error rather than a panic or silent success.
type Handler interface {
	ClusterConsole(ctx context.Context, req ClusterRequest) error
	MachineConsole(ctx context.Context, req MachineRequest) error
}

var handlers = map[vitiv1alpha1.KubernetesProviderType]Handler{}

// Register installs a handler for a provider. Called from each provider
// file's init() so imports alone wire up dispatch.
func Register(pt vitiv1alpha1.KubernetesProviderType, h Handler) {
	handlers[pt] = h
}

// ForProvider returns the handler for a given provider, or an error if
// none is registered.
func ForProvider(pt vitiv1alpha1.KubernetesProviderType) (Handler, error) {
	if pt == "" {
		return nil, fmt.Errorf("cluster has no provider set on spec.data.provider")
	}
	h, ok := handlers[pt]
	if !ok {
		return nil, fmt.Errorf("console not implemented for provider %q", pt)
	}
	return h, nil
}
