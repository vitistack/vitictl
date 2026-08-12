package pluginmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearTokenEnv removes any ambient GitHub token so a developer's own
// environment cannot influence the result.
func clearTokenEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	// Keep the gh CLI out of the picture: point PATH at an empty dir so
	// `gh auth token` cannot be found.
	t.Setenv("PATH", t.TempDir())
}

func TestTokenPrefersGHTokenOverGitHubToken(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GITHUB_TOKEN", "from-github-token")
	t.Setenv("GH_TOKEN", "from-gh-token")

	if got := token(); got != "from-gh-token" {
		t.Errorf("token() = %q, want GH_TOKEN to win", got)
	}
}

func TestTokenFallsBackToGitHubToken(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GITHUB_TOKEN", "from-github-token")

	if got := token(); got != "from-github-token" {
		t.Errorf("token() = %q, want GITHUB_TOKEN", got)
	}
}

// With no environment token, the gh CLI is consulted — that is what most
// developers actually have configured.
func TestTokenFallsBackToGhCli(t *testing.T) {
	clearTokenEnv(t)
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	script := "#!/bin/sh\nif [ \"$1\" = \"auth\" ] && [ \"$2\" = \"token\" ]; then echo from-gh-cli; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(gh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if got := token(); got != "from-gh-cli" {
		t.Errorf("token() = %q, want the gh CLI token", got)
	}
}

func TestTokenEmptyWhenNothingConfigured(t *testing.T) {
	clearTokenEnv(t)
	if got := token(); got != "" {
		t.Errorf("token() = %q, want empty when nothing is configured", got)
	}
}

// newAPI stands in for api.github.com, recording the Authorization header
// seen on each request.
type fakeAPI struct {
	authHeaders []string
	paths       []string
	// releaseStatus overrides the response for the release lookup.
	releaseStatus int
	tag           string
	assets        map[string]string // asset name -> body
}

func newFakeAPI(t *testing.T, tag string, assets map[string]string) (*fakeAPI, string) {
	t.Helper()
	f := &fakeAPI{tag: tag, assets: assets}
	ids := map[string]int{}
	i := 100
	for name := range assets {
		ids[name] = i
		i++
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
		f.paths = append(f.paths, r.URL.Path)

		if f.releaseStatus != 0 {
			w.WriteHeader(f.releaseStatus)
			return
		}

		// Asset download: /repos/o/r/releases/assets/<id>
		if id, ok := strings.CutPrefix(r.URL.Path, "/repos/o/r/releases/assets/"); ok {
			for name, assetID := range ids {
				if fmt.Sprintf("%d", assetID) == id {
					_, _ = w.Write([]byte(f.assets[name]))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// Release lookup by tag or "latest".
		if strings.HasPrefix(r.URL.Path, "/repos/o/r/releases/") {
			type asset struct {
				Name string `json:"name"`
				ID   int    `json:"id"`
			}
			out := struct {
				TagName string  `json:"tag_name"`
				Assets  []asset `json:"assets"`
			}{TagName: f.tag}
			for name, id := range ids {
				out.Assets = append(out.Assets, asset{Name: name, ID: id})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func withAPIBase(t *testing.T, url string) {
	t.Helper()
	old := githubAPIBase
	githubAPIBase = url
	t.Cleanup(func() { githubAPIBase = old })
}

func TestResolveLatestSendsBearerTokenWhenAvailable(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GH_TOKEN", "secret-token")
	fake, url := newFakeAPI(t, "v1.2.3", nil)
	withAPIBase(t, url)

	got, err := LatestVersion(context.Background(), "o/r")
	if err != nil {
		t.Fatalf("LatestVersion() error = %v", err)
	}
	if got != "v1.2.3" {
		t.Errorf("LatestVersion() = %q, want v1.2.3", got)
	}
	if len(fake.authHeaders) == 0 {
		t.Fatal("no requests reached the API")
	}
	if fake.authHeaders[0] != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want %q", fake.authHeaders[0], "Bearer secret-token")
	}
}

// Public repos must keep working exactly as before: no token, no header.
func TestResolveLatestSendsNoAuthHeaderWithoutToken(t *testing.T) {
	clearTokenEnv(t)
	fake, url := newFakeAPI(t, "v1.0.0", nil)
	withAPIBase(t, url)

	if _, err := LatestVersion(context.Background(), "o/r"); err != nil {
		t.Fatalf("LatestVersion() error = %v", err)
	}
	if fake.authHeaders[0] != "" {
		t.Errorf("Authorization = %q, want no header when unauthenticated", fake.authHeaders[0])
	}
}

// A 404 with no token is the private-repo case, and the message has to say
// so — GitHub deliberately returns 404 rather than 403 for private
// resources, so the bare status is misleading.
func TestResolveLatest404WithoutTokenExplainsPrivateRepos(t *testing.T) {
	clearTokenEnv(t)
	fake, url := newFakeAPI(t, "", nil)
	fake.releaseStatus = http.StatusNotFound
	withAPIBase(t, url)

	_, err := LatestVersion(context.Background(), "o/r")
	if err == nil {
		t.Fatal("LatestVersion() = nil error on 404, want one")
	}
	for _, want := range []string{"private", "GH_TOKEN", "gh auth login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// With a token present a 404 means something else — do not blame auth.
func TestResolveLatest404WithTokenDoesNotBlameAuth(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GH_TOKEN", "secret-token")
	fake, url := newFakeAPI(t, "", nil)
	fake.releaseStatus = http.StatusNotFound
	withAPIBase(t, url)

	_, err := LatestVersion(context.Background(), "o/r")
	if err == nil {
		t.Fatal("LatestVersion() = nil error on 404, want one")
	}
	if strings.Contains(err.Error(), "GH_TOKEN") {
		t.Errorf("error %q should not suggest setting a token when one is already set", err)
	}
	if !strings.Contains(err.Error(), "no releases") {
		t.Errorf("error %q should suggest the release may not exist", err)
	}
}

// Private release assets cannot be fetched from the plain browser URL, so an
// authenticated install must go through the releases API.
func TestFetchAssetUsesTheAssetAPIWhenAuthenticated(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GH_TOKEN", "secret-token")
	fake, url := newFakeAPI(t, "v1.0.0", map[string]string{"thing.tar.gz": "ARCHIVE-BODY"})
	withAPIBase(t, url)

	dst := filepath.Join(t.TempDir(), "thing.tar.gz")
	e := &Entry{Name: "x", Repo: "o/r"}
	if err := fetchAsset(context.Background(), e, "v1.0.0", "thing.tar.gz", dst); err != nil {
		t.Fatalf("fetchAsset() error = %v", err)
	}

	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ARCHIVE-BODY" {
		t.Errorf("downloaded %q, want ARCHIVE-BODY", body)
	}
	var sawAssetPath bool
	for _, p := range fake.paths {
		if strings.Contains(p, "/releases/assets/") {
			sawAssetPath = true
		}
	}
	if !sawAssetPath {
		t.Errorf("expected a /releases/assets/<id> request, got paths %v", fake.paths)
	}
	for i, h := range fake.authHeaders {
		if h != "Bearer secret-token" {
			t.Errorf("request %d Authorization = %q, want the bearer token", i, h)
		}
	}
}

func TestFetchAssetReportsAMissingAsset(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GH_TOKEN", "secret-token")
	_, url := newFakeAPI(t, "v1.0.0", map[string]string{"other.tar.gz": "x"})
	withAPIBase(t, url)

	e := &Entry{Name: "x", Repo: "o/r"}
	err := fetchAsset(context.Background(), e, "v1.0.0", "missing.tar.gz", filepath.Join(t.TempDir(), "out"))
	if err == nil {
		t.Fatal("fetchAsset() = nil error for a missing asset, want one")
	}
	if !strings.Contains(err.Error(), "missing.tar.gz") {
		t.Errorf("error %q should name the missing asset", err)
	}
}

// Without a token the download must still use the plain release URL, so
// public installs keep working and stay off the API rate limit.
func TestFetchAssetUsesBrowserURLWhenUnauthenticated(t *testing.T) {
	clearTokenEnv(t)
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unauthenticated download sent an Authorization header")
		}
		_, _ = w.Write([]byte("PUBLIC-BODY"))
	}))
	t.Cleanup(srv.Close)

	old := githubDownloadBase
	githubDownloadBase = srv.URL
	t.Cleanup(func() { githubDownloadBase = old })

	dst := filepath.Join(t.TempDir(), "thing.tar.gz")
	e := &Entry{Name: "x", Repo: "o/r"}
	if err := fetchAsset(context.Background(), e, "v1.0.0", "thing.tar.gz", dst); err != nil {
		t.Fatalf("fetchAsset() error = %v", err)
	}
	body, _ := os.ReadFile(dst)
	if string(body) != "PUBLIC-BODY" {
		t.Errorf("downloaded %q, want PUBLIC-BODY", body)
	}
	if gotPath != "/o/r/releases/download/v1.0.0/thing.tar.gz" {
		t.Errorf("path = %q, want the plain release download URL", gotPath)
	}
}

// A stale or wrong token must not break installs that would work anonymously.
// Public repos are readable without credentials, so a rejected token should
// fall back rather than fail — otherwise a leftover GH_TOKEN in a shell
// profile breaks every public plugin install.
func TestRejectedTokenFallsBackToAnonymousForPublicRepos(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GH_TOKEN", "stale-token")

	var sawAuthed, sawAnon bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuthed = true
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sawAnon = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0"}`))
	}))
	t.Cleanup(srv.Close)
	withAPIBase(t, srv.URL)

	var warnings strings.Builder
	oldWarn := warnOut
	warnOut = &warnings
	t.Cleanup(func() { warnOut = oldWarn })

	got, err := LatestVersion(context.Background(), "o/r")
	if err != nil {
		t.Fatalf("LatestVersion() error = %v, want an anonymous retry to succeed", err)
	}
	if got != "v2.0.0" {
		t.Errorf("LatestVersion() = %q, want v2.0.0", got)
	}
	if !sawAuthed || !sawAnon {
		t.Errorf("expected an authenticated attempt then an anonymous retry (authed=%v anon=%v)", sawAuthed, sawAnon)
	}
	if !strings.Contains(warnings.String(), "rejected") {
		t.Errorf("warning = %q, want it to say the token was rejected", warnings.String())
	}
}

// When the anonymous retry also fails the repo really is inaccessible, and
// the token error is the useful one to report.
func TestRejectedTokenOnPrivateRepoReportsTheAuthError(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GH_TOKEN", "stale-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound) // private repo, anonymously invisible
	}))
	t.Cleanup(srv.Close)
	withAPIBase(t, srv.URL)

	var warnings strings.Builder
	oldWarn := warnOut
	warnOut = &warnings
	t.Cleanup(func() { warnOut = oldWarn })

	_, err := LatestVersion(context.Background(), "o/r")
	if err == nil {
		t.Fatal("LatestVersion() = nil error, want the token rejection reported")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error %q should report the token rejection, not the anonymous 404", err)
	}
}

// The release lookup and the asset download are separate requests, so the
// fallback has to cover both — otherwise a stale token gets past the lookup
// and then fails on the download.
func TestRejectedTokenFallsBackForTheAssetDownloadToo(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GH_TOKEN", "stale-token")

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v1.0.0","assets":[{"name":"thing.tar.gz","id":7}]}`))
	}))
	t.Cleanup(api.Close)
	withAPIBase(t, api.URL)

	var servedPlainURL bool
	dl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servedPlainURL = true
		_, _ = w.Write([]byte("FALLBACK-BODY"))
	}))
	t.Cleanup(dl.Close)
	oldDL := githubDownloadBase
	githubDownloadBase = dl.URL
	t.Cleanup(func() { githubDownloadBase = oldDL })

	var warnings strings.Builder
	oldWarn := warnOut
	warnOut = &warnings
	t.Cleanup(func() { warnOut = oldWarn })

	dst := filepath.Join(t.TempDir(), "thing.tar.gz")
	e := &Entry{Name: "x", Repo: "o/r"}
	if err := fetchAsset(context.Background(), e, "v1.0.0", "thing.tar.gz", dst); err != nil {
		t.Fatalf("fetchAsset() error = %v, want a fallback to the public download URL", err)
	}
	body, _ := os.ReadFile(dst)
	if string(body) != "FALLBACK-BODY" {
		t.Errorf("downloaded %q, want the fallback body", body)
	}
	if !servedPlainURL {
		t.Error("expected the plain release download URL to be used after the token was refused")
	}
}

// A private index is only usable if the index fetch authenticates too — but a
// token must never be sent to an arbitrary host the user pointed us at.
func TestShouldAuthenticateOnlyForGitHubHosts(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://api.github.com/repos/o/r/releases/latest", true},
		{"https://raw.githubusercontent.com/o/r/main/plugins.yaml", true},
		{"https://github.com/o/r/releases/download/v1/x.tar.gz", true},
		{"https://internal.example.com/plugins.yaml", false},
		{"http://127.0.0.1:8080/plugins.yaml", false},
		{"https://evil.example.com/github.com/plugins.yaml", false},
		// Suffix matching must not be fooled by a lookalike domain.
		{"https://notgithub.com/plugins.yaml", false},
		{"https://github.com.evil.example/plugins.yaml", false},
		{"::not a url::", false},
	}
	for _, tt := range tests {
		if got := shouldAuthenticate(tt.url); got != tt.want {
			t.Errorf("shouldAuthenticate(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

// The index may be hosted anywhere; sending credentials there would leak them.
func TestFetchIndexSendsNoTokenToNonGitHubHosts(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GH_TOKEN", "secret-token")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("plugins:\n  - name: x\n    repo: o/r\n"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv(IndexEnvVar, srv.URL)

	idx, err := FetchIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchIndex() error = %v", err)
	}
	if len(idx.Plugins) != 1 {
		t.Errorf("got %d plugins, want 1", len(idx.Plugins))
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want no credentials sent to a third-party host", gotAuth)
	}
}

// A stale token makes raw.githubusercontent.com answer 404 rather than 401 for
// a public file, so the index fetch must retry anonymously on any failure —
// otherwise a leftover token breaks every plugin command, not just installs.
func TestFetchIndexRetriesAnonymouslyWhenTheTokenBreaksIt(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GH_TOKEN", "stale-token")

	var authedAttempts, anonAttempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			authedAttempts++
			w.WriteHeader(http.StatusNotFound) // what raw.githubusercontent does
			return
		}
		anonAttempts++
		_, _ = w.Write([]byte("plugins:\n  - name: x\n    repo: o/r\n"))
	}))
	t.Cleanup(srv.Close)
	// Make the fake host trusted so the first attempt actually carries auth.
	withAPIBase(t, srv.URL)
	t.Setenv(IndexEnvVar, srv.URL)

	idx, err := FetchIndex(context.Background())
	if err != nil {
		t.Fatalf("FetchIndex() error = %v, want an anonymous retry to succeed", err)
	}
	if len(idx.Plugins) != 1 || idx.Plugins[0].Name != "x" {
		t.Errorf("got %+v, want the index from the anonymous retry", idx.Plugins)
	}
	if authedAttempts != 1 || anonAttempts != 1 {
		t.Errorf("attempts: authed=%d anon=%d, want exactly one of each", authedAttempts, anonAttempts)
	}
}

// A genuinely unreachable index still reports an error rather than hanging or
// silently returning nothing.
func TestFetchIndexReportsAFailureWhenBothAttemptsFail(t *testing.T) {
	clearTokenEnv(t)
	t.Setenv("GH_TOKEN", "stale-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	withAPIBase(t, srv.URL)
	t.Setenv(IndexEnvVar, srv.URL)

	if _, err := FetchIndex(context.Background()); err == nil {
		t.Fatal("FetchIndex() = nil error when the index is unreachable, want one")
	}
}
