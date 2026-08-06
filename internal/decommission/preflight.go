package decommission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
)

// Preflight verifies everything a full decommission needs — without changing
// anything. It prints one line per check and returns an error when any
// blocking check fails, so callers can exit non-zero. Intended for
// "viti kc delete --dry-run".
func (r *Runner) Preflight(ctx context.Context) error {
	ok := true
	pass := func(format string, args ...any) { r.printf("✅ "+format, args...) }
	fail := func(format string, args ...any) { ok = false; r.printf("❌ "+format, args...) }
	note := func(format string, args ...any) { r.printf("ℹ️  "+format, args...) }

	// Management side: the CR and its machines.
	var kc vitiv1alpha1.KubernetesCluster
	if err := r.mgmt.Get(ctx, ctrlclient.ObjectKey{Namespace: r.namespace, Name: r.cluster.Name}, &kc); err != nil {
		fail("KubernetesCluster %s/%s: %v", r.namespace, r.cluster.Name, err)
	} else {
		pass("KubernetesCluster %s/%s (clusterId %s, phase %s)", r.namespace, kc.Name, r.clusterID, kc.Status.Phase)
	}
	if n, err := r.countClusterMachines(ctx); err != nil {
		fail("machines: %v", err)
	} else {
		pass("machines: %d found for clusterId %s", n, r.clusterID)
	}

	// Guest side (skipped when --skip-preclean).
	if r.opts.SkipPreclean {
		note("preclean will be SKIPPED — guest checks not performed; external state (IPAM, DNS, volumes, ROR) will leak")
	} else if r.guest == nil {
		fail("guest client unavailable (kubeconfig secret missing or unparsable)")
	} else {
		gctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var nss corev1.NamespaceList
		if err := r.guest.List(gctx, &nss, ctrlclient.Limit(1)); err != nil {
			fail("guest API unreachable: %v", err)
		} else {
			pass("guest API reachable (via kubeconfig from the cluster's own secret)")
			r.preflightROR(gctx, pass, fail, note)
		}
	}

	if !ok {
		return fmt.Errorf("preflight failed — fix the ❌ items above before decommissioning")
	}
	r.printf("🟢 preflight clean — a real run has everything it needs")
	return nil
}

// preflightROR checks the ROR half without purging anything: identity secret
// present, local token valid, and the registry entry consistent with the
// secret. 404 (already purged) passes — the real run treats it as done.
func (r *Runner) preflightROR(ctx context.Context, pass, fail, note func(string, ...any)) {
	var ns corev1.Namespace
	if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Name: "nhn-ror"}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			note("no nhn-ror namespace — ROR purge will be skipped")
			return
		}
		fail("checking nhn-ror namespace: %v", err)
		return
	}
	var secret corev1.Secret
	if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Namespace: "nhn-ror", Name: "ror-apikey"}, &secret); err != nil {
		fail("secret nhn-ror/ror-apikey: %v", err)
		return
	}
	clusterID := string(secret.Data["CLUSTER_ID"])
	uid := string(secret.Data["CLUSTER_UID"])
	if uid == "" {
		fail("secret nhn-ror/ror-apikey has no CLUSTER_UID")
		return
	}
	pass("ROR identity: clusterid=%s uid=%s", clusterID, uid)

	if _, err := r.rorToken(); err != nil {
		fail("ROR token: %v", err)
		return
	}
	code, body, err := r.rorRequest(ctx, http.MethodGet, "/v1/clusters/"+clusterID)
	switch {
	case err != nil:
		fail("ROR API unreachable: %v", err)
	case code == http.StatusOK:
		var got struct {
			UID string `json:"uid"`
		}
		if jerr := json.Unmarshal(body, &got); jerr != nil || got.UID == "" {
			fail("ROR returned 200 for %s but no uid", clusterID)
		} else if got.UID != uid {
			fail("ROR uid mismatch: registry has %s, secret says %s", got.UID, uid)
		} else {
			pass("ROR registry entry verified (uid matches secret)")
		}
	case code == http.StatusNotFound:
		note("cluster not found in ROR — already purged, real run will treat as done")
	case code == http.StatusUnauthorized:
		fail("ROR token expired/invalid — run 'ror login' first")
	default:
		fail("ROR verify GET returned HTTP %d: %s", code, strings.TrimSpace(string(body)))
	}
}
