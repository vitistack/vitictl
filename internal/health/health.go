// Package health probes a target Kubernetes cluster's API server health
// endpoints (/readyz, /livez, /healthz) and node readiness, using the
// cluster's own kube.config so no prior "viti kc login" is required.
//
// It is the lightweight, provider-agnostic counterpart to the deep
// "talosctl health" check used by "viti kc health --full" on Talos
// clusters (see internal/talos).
package health

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// DefaultTimeout caps a single health probe so an unreachable API server
// fails fast instead of hanging the command (and, for health-all, the whole
// batch). Callers can override by passing a context with their own deadline.
const DefaultTimeout = 15 * time.Second

// EndpointResult is the outcome of probing one API-server health endpoint.
type EndpointResult struct {
	// Path is the endpoint that was probed, e.g. "/readyz".
	Path string
	// OK reports whether the endpoint returned HTTP 200.
	OK bool
	// Body is the (verbose) response body. For a failed readyz/livez this
	// lists the individual checks that failed, which is the useful part.
	Body string
	// Err is set only when the server could not be reached at all (no HTTP
	// status). A non-200 response is reported via OK/Body, not Err.
	Err error
}

// NodeSummary captures node readiness for a cluster.
type NodeSummary struct {
	Total    int
	Ready    int
	NotReady []string // names of nodes not in Ready=True
}

// AllReady reports whether every node is Ready (and at least one exists).
func (s NodeSummary) AllReady() bool { return s.Total > 0 && len(s.NotReady) == 0 }

// RESTConfigFromKubeconfig builds a *rest.Config from raw kubeconfig bytes
// (the secret's "kube.config" entry), applying DefaultTimeout so probes
// can't hang indefinitely.
func RESTConfigFromKubeconfig(kubeconfig []byte) (*rest.Config, error) {
	if len(kubeconfig) == 0 {
		return nil, fmt.Errorf("empty kubeconfig")
	}
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	return cfg, nil
}

// CheckEndpoint probes a single API-server health endpoint (e.g. "/readyz",
// "/livez", "/healthz"). When verbose is true the ?verbose query parameter
// is added so the response enumerates each individual check.
//
// The body is captured regardless of status code; OK reflects HTTP 200.
// Err is populated only for transport-level failures (the request never got
// an HTTP status back) — a 500 with a useful verbose body is reported via
// OK=false + Body, not Err.
func CheckEndpoint(ctx context.Context, cfg *rest.Config, path string, verbose bool) EndpointResult {
	res := EndpointResult{Path: path}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		res.Err = err
		return res
	}
	req := cs.Discovery().RESTClient().Get().AbsPath(path)
	if verbose {
		req = req.Param("verbose", "")
	}
	var code int
	raw, rawErr := req.Do(ctx).StatusCode(&code).Raw()
	res.Body = string(raw)
	res.OK = code == 200
	if code == 0 {
		// Never reached the server (DNS, dial, TLS, timeout) — surface it.
		res.Err = rawErr
	}
	return res
}

// CheckNodes lists the cluster's nodes and summarizes Ready status.
func CheckNodes(ctx context.Context, cfg *rest.Config) (NodeSummary, error) {
	var s NodeSummary
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return s, err
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return s, fmt.Errorf("listing nodes: %w", err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		s.Total++
		if nodeReady(n) {
			s.Ready++
		} else {
			s.NotReady = append(s.NotReady, n.Name)
		}
	}
	return s, nil
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// FirstLine returns the first non-empty line of s, trimmed — handy for
// condensing a verbose health body into a one-line table message.
func FirstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// FailedChecks parses a verbose /readyz|/livez|/healthz body and returns the
// names of the individual checks that failed. The apiserver renders each
// check as "[+]name ok" (passing) or "[-]name failed: …" (failing); we keep
// the names of the "[-]" lines. Returns nil when nothing failed (or the body
// isn't in the verbose check format).
func FailedChecks(body string) []string {
	var failed []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[-]") {
			continue
		}
		name := strings.TrimPrefix(line, "[-]")
		if i := strings.IndexAny(name, " \t"); i >= 0 {
			name = name[:i]
		}
		if name != "" {
			failed = append(failed, name)
		}
	}
	return failed
}
