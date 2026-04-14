package kube

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/settings"
)

// RequiredCRDs is the list of CRDs that must be installed on every
// availability zone cluster before viti can operate against it.
var RequiredCRDs = []string{
	"vitistacks.vitistack.io",
	"kubernetesclusters.vitistack.io",
	"machines.vitistack.io",
}

// Client bundles a controller-runtime client with its originating
// availability zone.
type Client struct {
	AZ         settings.AvailabilityZone
	Ctrl       ctrlclient.Client
	RESTConfig *rest.Config
}

// ResolveAvailabilityZones returns the availability zones to operate
// against. If `name` is non-empty it resolves that single named zone.
// Otherwise it returns all configured zones, and errors if none are
// configured.
func ResolveAvailabilityZones(name string) ([]settings.AvailabilityZone, error) {
	if name != "" {
		z, err := settings.AvailabilityZoneByName(name)
		if err != nil {
			return nil, err
		}
		return []settings.AvailabilityZone{z}, nil
	}
	zones, err := settings.AvailabilityZones()
	if err != nil {
		return nil, err
	}
	if len(zones) == 0 {
		return nil, errNoAvailabilityZones()
	}
	return zones, nil
}

// Connect builds a Client for a single availability zone, verifying the
// required CRDs.
func Connect(ctx context.Context, az settings.AvailabilityZone) (*Client, error) {
	cfg, err := buildRESTConfig(az)
	if err != nil {
		return nil, fmt.Errorf("availability zone %q: %w", az.Name, err)
	}

	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("adding core scheme: %w", err)
	}
	if err := apiextv1.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("adding apiextensions scheme: %w", err)
	}
	if err := vitiv1alpha1.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("adding vitistack scheme: %w", err)
	}

	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: sch})
	if err != nil {
		return nil, fmt.Errorf("availability zone %q: building kube client: %w", az.Name, err)
	}
	if err := ensureCRDs(ctx, c); err != nil {
		return nil, fmt.Errorf("availability zone %q: %w", az.Name, err)
	}
	return &Client{AZ: az, Ctrl: c, RESTConfig: cfg}, nil
}

// ConnectAll builds clients for every availability zone. If allowPartial is
// true, a failing zone is reported to `warn` and skipped rather than
// aborting.
func ConnectAll(ctx context.Context, zones []settings.AvailabilityZone, allowPartial bool, warn func(error)) ([]*Client, error) {
	clients := make([]*Client, 0, len(zones))
	for _, z := range zones {
		c, err := Connect(ctx, z)
		if err != nil {
			if allowPartial {
				if warn != nil {
					warn(err)
				}
				continue
			}
			return nil, err
		}
		clients = append(clients, c)
	}
	if len(clients) == 0 {
		return nil, fmt.Errorf("no reachable availability zones (configured %d)", len(zones))
	}
	return clients, nil
}

func buildRESTConfig(az settings.AvailabilityZone) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if az.Kubeconfig != "" {
		if _, err := os.Stat(az.Kubeconfig); err != nil {
			return nil, fmt.Errorf("kubeconfig %q is not readable: %w", az.Kubeconfig, err)
		}
		loadingRules.ExplicitPath = az.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if az.Context != "" {
		overrides.CurrentContext = az.Context
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kube client config: %w", err)
	}
	return cfg, nil
}

func ensureCRDs(ctx context.Context, c ctrlclient.Client) error {
	var missing []string
	for _, name := range RequiredCRDs {
		var crd apiextv1.CustomResourceDefinition
		err := c.Get(ctx, types.NamespacedName{Name: name}, &crd)
		if err != nil {
			if apierrors.IsNotFound(err) {
				missing = append(missing, name)
				continue
			}
			return fmt.Errorf("checking CRD %s: %w", name, err)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"availability zone cluster is missing required Vitistack CRDs: %s (install them from vitistack/common charts or crds/ before using this command)",
		strings.Join(missing, ", "),
	)
}

func errNoAvailabilityZones() error {
	return errors.New(strings.TrimSpace(`
no availability zones configured.

Configure at least one kubeconfig/context availability zone with one of:
  viti config init
  viti config add <name> --kubeconfig <path>
  viti config add <name> --context <kubectl-context>`))
}
