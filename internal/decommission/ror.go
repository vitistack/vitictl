package decommission

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// rorPurge tracks the background ROR deregistration. It is started the
// moment the ROR agents are scaled down so the ROR API's 10-minute
// inactivity guard counts down while the rest of the preclean runs; the
// result is collected before the phase-1 verdict.
//
// The purge sends ?force=true once the agent pods are verified gone — a
// stronger liveness check than the API's timestamp heuristic (which also
// counts heartbeats from management-plane reporters outside the guest).
// force is supported since ror-api v1.14.10; older APIs ignore it and the
// 409 retry loop applies.
type rorPurge struct {
	clusterID string
	uid       string
	done      chan struct{}
	ok        bool
	log       []string
}

func (r *Runner) rorConfigPath() string {
	if r.opts.RORConfigPath != "" {
		return r.opts.RORConfigPath
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ror", "config.yaml")
}

// rorToken re-reads the access token on every call so an externally
// refreshed token (ror login in another terminal) is picked up mid-run.
func (r *Runner) rorToken() (string, error) {
	raw, err := os.ReadFile(r.rorConfigPath())
	if err != nil {
		return "", err
	}
	var cfg struct {
		RorAuth struct {
			OIDCConfig struct {
				AccessToken string `json:"accesstoken"`
			} `json:"oidcconfig"`
		} `json:"rorauth"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("parsing %s: %w", r.rorConfigPath(), err)
	}
	if cfg.RorAuth.OIDCConfig.AccessToken == "" {
		return "", fmt.Errorf("no rorauth.oidcconfig.accesstoken in %s — run 'ror login'", r.rorConfigPath())
	}
	return cfg.RorAuth.OIDCConfig.AccessToken, nil
}

// startRORPurge reads the cluster's ROR identity from its own secret,
// scales the ROR agents to zero, and launches the purge in the background.
func (r *Runner) startRORPurge(ctx context.Context) {
	var ns corev1.Namespace
	if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Name: "nhn-ror"}, &ns); err != nil {
		r.printf("No nhn-ror namespace found, skipping ROR steps")
		return
	}
	r.printf("Stop ROR cluster agents (starts the ROR inactivity clock)")

	var secret corev1.Secret
	if err := r.guest.Get(ctx, ctrlclient.ObjectKey{Namespace: "nhn-ror", Name: "ror-apikey"}, &secret); err != nil {
		r.warnf("could not read secret nhn-ror/ror-apikey — ROR purge will be skipped: %v", err)
		return
	}
	clusterID := string(secret.Data["CLUSTER_ID"])
	uid := string(secret.Data["CLUSTER_UID"])

	var deps appsv1.DeploymentList
	if err := r.guest.List(ctx, &deps, ctrlclient.InNamespace("nhn-ror")); err == nil {
		for i := range deps.Items {
			if err := r.scaleTo(ctx, &deps.Items[i], 0); err != nil {
				r.warnf("failed to scale down %s: %v", deps.Items[i].Name, err)
			}
		}
	}

	if uid == "" {
		r.failf("secret nhn-ror/ror-apikey has no CLUSTER_UID — ROR purge skipped")
		return
	}
	if _, err := r.rorToken(); err != nil {
		r.failf("ROR purge unavailable: %v", err)
		r.printf("  Purge manually: curl -X DELETE -H \"Authorization: Bearer $TOKEN\" %s/v1/clusters/uid/%s", r.opts.RORAPIURL, uid)
		return
	}

	p := &rorPurge{clusterID: clusterID, uid: uid, done: make(chan struct{})}
	r.ror = p
	r.printf("  ROR identity from secret nhn-ror/ror-apikey: clusterid=%s uid=%s", clusterID, uid)
	r.printf("  Starting ROR purge in the background (rides out the inactivity guard while cleanup continues)")
	go r.runRORPurge(ctx, p)
}

func (p *rorPurge) logf(format string, args ...any) {
	p.log = append(p.log, fmt.Sprintf(format, args...))
}

func (r *Runner) runRORPurge(ctx context.Context, p *rorPurge) {
	defer close(p.done)

	// Wait for the agent pods to actually terminate, then purge with force.
	force := ""
	for range 12 {
		var pods corev1.PodList
		if err := r.guest.List(ctx, &pods, ctrlclient.InNamespace("nhn-ror")); err == nil && len(pods.Items) == 0 {
			force = "?force=true"
			break
		}
		select {
		case <-ctx.Done():
			p.logf("WARNING: context cancelled while waiting for ROR agent pods")
			return
		case <-time.After(5 * time.Second):
		}
	}
	if force != "" {
		p.logf("ROR agent pods confirmed gone — purging with force=true (skips inactivity guard where supported)")
	} else {
		p.logf("WARNING: pods still present in nhn-ror — purging WITHOUT force (inactivity guard applies)")
	}

	// Safety: confirm the uid in ROR matches the uid from the cluster's own
	// secret. Branch on the HTTP status — an expired token (401) must not be
	// mistaken for "not found / already purged".
	code, body, err := r.rorRequest(ctx, http.MethodGet, "/v1/clusters/"+p.clusterID)
	if err != nil {
		p.logf("WARNING: could not verify cluster in ROR: %v", err)
		return
	}
	switch code {
	case http.StatusOK:
		var got struct {
			UID string `json:"uid"`
		}
		if err := json.Unmarshal(body, &got); err != nil || got.UID == "" {
			p.logf("WARNING: ROR returned 200 for %s but no uid — NOT purging", p.clusterID)
			return
		}
		if got.UID != p.uid {
			p.logf("WARNING: uid mismatch — ROR has %s, secret says %s. NOT purging.", got.UID, p.uid)
			return
		}
	case http.StatusNotFound:
		p.logf("Cluster %s not found in ROR — already purged, nothing to do", p.clusterID)
		p.ok = true
		return
	case http.StatusUnauthorized:
		p.logf("WARNING: ROR token expired/invalid — refresh with 'ror login' and re-run")
		return
	default:
		p.logf("WARNING: unexpected HTTP %d from ROR on verify GET: %s", code, truncate(body, 200))
		return
	}

	for attempt := 1; attempt <= 20; attempt++ {
		code, body, err := r.rorRequest(ctx, http.MethodDelete, "/v1/clusters/uid/"+p.uid+force)
		if err != nil {
			p.logf("WARNING: ROR purge request failed: %v", err)
			return
		}
		switch code {
		case http.StatusOK:
			p.logf("Purged from ROR: %s", truncate(body, 300))
			p.ok = true
			return
		case http.StatusNotFound:
			p.logf("Already gone from ROR")
			p.ok = true
			return
		case http.StatusUnauthorized:
			p.logf("WARNING: ROR token expired/invalid — refresh with 'ror login' and re-run")
			return
		case http.StatusConflict:
			p.logf("attempt %d/20: ROR says cluster reported recently (409) — retrying in 60s", attempt)
			select {
			case <-ctx.Done():
				return
			case <-time.After(60 * time.Second):
			}
		default:
			p.logf("WARNING: unexpected HTTP %d from ROR: %s", code, truncate(body, 200))
			return
		}
	}
	p.logf("WARNING: cluster NOT purged from ROR after 20 attempts")
}

func (r *Runner) rorRequest(ctx context.Context, method, path string) (int, []byte, error) {
	token, err := r.rorToken()
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, r.opts.RORAPIURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// collectRORResult waits for the background purge and folds its log and
// verdict into the run.
func (r *Runner) collectRORResult(ctx context.Context) {
	if r.ror == nil {
		return
	}
	r.printf("Deregister cluster from ROR (%s)", r.opts.RORAPIURL)
	select {
	case <-r.ror.done:
	case <-time.After(25 * time.Minute):
		r.failf("ROR purge did not finish in time")
		return
	case <-ctx.Done():
		r.failf("context cancelled while waiting for ROR purge")
		return
	}
	for _, line := range r.ror.log {
		r.printf("  %s", line)
	}
	if !r.ror.ok {
		r.failed = true
	}
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
