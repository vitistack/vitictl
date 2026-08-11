package pluginmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// githubAPIBase and githubDownloadBase are variables rather than constants
// so tests can point them at a local server.
var (
	githubAPIBase      = "https://api.github.com"
	githubDownloadBase = "https://github.com"
	// warnOut receives non-fatal notices; a variable so tests can capture it.
	warnOut io.Writer = os.Stderr
)

// token returns a GitHub token for authenticating release lookups and asset
// downloads, or "" when none is configured.
//
// Only private repositories need one. GitHub answers unauthenticated requests
// for private resources with 404 rather than 403, so without this the failure
// is indistinguishable from "no such release".
//
// The lookup order matches the gh CLI's own: GH_TOKEN, then GITHUB_TOKEN,
// then whatever `gh auth token` reports — which is what most developers
// actually have set up.
func token() string {
	for _, env := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	// #nosec G204 -- gh is resolved from PATH and invoked with fixed arguments.
	out, err := exec.Command(gh, "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// newAPIRequest builds a GitHub API request, authenticated when authorize is
// true and a token is available.
func newAPIRequest(ctx context.Context, url, accept string, authorize bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if authorize && shouldAuthenticate(url) {
		if t := token(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	return req, nil
}

// githubHosts are the hosts a token may be sent to. The plugin index URL is
// user-supplied (VITICTL_PLUGINS_INDEX) and could point anywhere, so
// credentials are withheld from every other host.
var githubHosts = []string{"github.com", "api.github.com", "raw.githubusercontent.com", "objects.githubusercontent.com"}

// shouldAuthenticate reports whether url may carry the user's token.
//
// GitHub's own hosts qualify, as do the configured API and download bases —
// those are this package's endpoints rather than anything a user pointed us
// at, and overriding them is how the tests run against a local server.
// Matching is on the exact host, never a suffix, so lookalikes such as
// notgithub.com or github.com.evil.example do not qualify.
func shouldAuthenticate(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()

	trusted := make([]string, 0, len(githubHosts)+2)
	trusted = append(trusted, githubHosts...)
	for _, base := range []string{githubAPIBase, githubDownloadBase} {
		if b, err := url.Parse(base); err == nil && b.Hostname() != "" {
			trusted = append(trusted, b.Hostname())
		}
	}
	for _, h := range trusted {
		if strings.EqualFold(host, h) {
			return true
		}
	}
	return false
}

// rejectedAuth reports whether a response means the token was refused.
func rejectedAuth(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden
}

// release is the subset of GitHub's release payload the installer needs.
type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		ID   int64  `json:"id"`
	} `json:"assets"`
}

// fetchRelease retrieves a release by tag, or the latest when tag is empty.
func fetchRelease(ctx context.Context, repo, tag string) (*release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", githubAPIBase, repo)
	if tag != "" {
		url = fmt.Sprintf("%s/repos/%s/releases/tags/%s", githubAPIBase, repo, tag)
	}
	body, err := getWithAnonymousFallback(ctx, url, "application/vnd.github+json", repo)
	if err != nil {
		return nil, err
	}
	var out release
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// getWithAnonymousFallback performs an authenticated GET, retrying once
// without credentials if the token is refused.
//
// A stale GH_TOKEN left in a shell profile would otherwise break installs of
// public plugins that need no credentials at all. If the anonymous retry also
// fails then the resource really is out of reach, and the original
// authentication error is the more useful one to report.
func getWithAnonymousFallback(ctx context.Context, url, accept, repo string) ([]byte, error) {
	body, code, status, err := getOnce(ctx, url, accept, true)
	if err != nil {
		return nil, err
	}
	if code == http.StatusOK {
		return body, nil
	}
	if !rejectedAuth(code) || token() == "" {
		return nil, describeReleaseError(code, status, repo)
	}

	authErr := describeReleaseError(code, status, repo)
	body, retryCode, _, err := getOnce(ctx, url, accept, false)
	if err != nil || retryCode != http.StatusOK {
		return nil, authErr
	}
	_, _ = fmt.Fprintf(warnOut, "==> ⚠️  GitHub rejected the configured token (%s); continuing without authentication\n", status)
	return body, nil
}

// getOnce issues a single GET and returns the body with the response status.
func getOnce(ctx context.Context, url, accept string, authorize bool) ([]byte, int, string, error) {
	req, err := newAPIRequest(ctx, url, accept, authorize)
	if err != nil {
		return nil, 0, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, resp.Status, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, "", err
	}
	return body, resp.StatusCode, resp.Status, nil
}

// describeReleaseError turns a failed release lookup into something the user
// can act on. A 404 is ambiguous: GitHub returns it both for a repo that has
// no releases and for one the caller cannot see, so the advice depends on
// whether a token was in play.
func describeReleaseError(code int, status, repo string) error {
	switch {
	case code == http.StatusNotFound && token() == "":
		return fmt.Errorf(
			"github API returned 404 for %s — the repository may be private, or it has no releases yet. "+
				"For a private repository, authenticate first: set GH_TOKEN (or GITHUB_TOKEN), or run 'gh auth login'",
			repo)
	case code == http.StatusNotFound:
		return fmt.Errorf(
			"github API returned 404 for %s — no releases found. "+
				"Check the tag exists and that your token can read the repository",
			repo)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf(
			"github API returned %s for %s — the token was rejected or lacks access; "+
				"re-authenticate with 'gh auth login' or refresh GH_TOKEN",
			status, repo)
	default:
		return fmt.Errorf("github API returned %s for %s", status, repo)
	}
}

// fetchAsset downloads one release asset to dst.
//
// Authenticated callers go through the releases API, because private release
// assets are not reachable from the plain browser download URL. Without a
// token the browser URL is used, which keeps public installs working exactly
// as before and off the API's unauthenticated rate limit.
func fetchAsset(ctx context.Context, entry *Entry, version, asset, dst string) error {
	publicURL := fmt.Sprintf("%s/%s/releases/download/%s/%s",
		githubDownloadBase, entry.Repo, version, asset)
	if token() == "" {
		return download(ctx, publicURL, dst)
	}

	rel, err := fetchRelease(ctx, entry.Repo, version)
	if err != nil {
		return err
	}
	for _, a := range rel.Assets {
		if a.Name != asset {
			continue
		}
		url := fmt.Sprintf("%s/repos/%s/releases/assets/%d", githubAPIBase, entry.Repo, a.ID)
		req, err := newAPIRequest(ctx, url, "application/octet-stream", true)
		if err != nil {
			return err
		}
		code, err := downloadRequest(req, dst)
		if err != nil && rejectedAuth(code) {
			// The token was refused for the download as well; the public URL
			// still works for public repositories.
			return download(ctx, publicURL, dst)
		}
		return err
	}
	return fmt.Errorf("release %s of %s has no asset named %s", version, entry.Repo, asset)
}
