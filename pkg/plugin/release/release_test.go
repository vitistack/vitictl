package release

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// withAPI points the package at a local server and blanks every credential
// source, so a developer's real gh login can never leak into a test.
func withAPI(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// The release-page fallback goes to the same server, so a test can never
	// reach the real github.com.
	oldAPI, oldWeb := githubAPIBase, githubWebBase
	githubAPIBase, githubWebBase = srv.URL, srv.URL
	t.Cleanup(func() { githubAPIBase, githubWebBase = oldAPI, oldWeb })

	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	// Token() falls back to `gh auth token`. An empty PATH makes that lookup
	// fail, so a test that means "unauthenticated" really is unauthenticated
	// even on a machine where gh is logged in.
	t.Setenv("PATH", t.TempDir())
}

func serveJSON(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const latestBody = `{"tag_name":"v1.2.3","name":"v1.2.3","html_url":"https://example.test/releases/v1.2.3"}`

const testRepo = "vitistack/example"

func TestFetchLatestReturnsTheRelease(t *testing.T) {
	withAPI(t, serveJSON(http.StatusOK, latestBody))

	got, err := FetchLatest(context.Background(), testRepo)
	if err != nil {
		t.Fatalf("FetchLatest() error = %v", err)
	}
	if got.Tag != "v1.2.3" {
		t.Errorf("Tag = %q, want v1.2.3", got.Tag)
	}
	if got.URL != "https://example.test/releases/v1.2.3" {
		t.Errorf("URL = %q, want the release page", got.URL)
	}
}

func TestFetchLatestRequestsTheLatestReleaseOfTheRepo(t *testing.T) {
	var path string
	withAPI(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(latestBody))
	})

	if _, err := FetchLatest(context.Background(), testRepo); err != nil {
		t.Fatalf("FetchLatest() error = %v", err)
	}
	if want := "/repos/" + testRepo + "/releases/latest"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// The private repository is the whole reason this package exists: without a
// token every lookup 404s, so the token must actually be sent.
func TestFetchLatestAuthenticatesWhenATokenIsSet(t *testing.T) {
	var auth string
	withAPI(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(latestBody))
	})
	t.Setenv("GH_TOKEN", "s3cret")

	if _, err := FetchLatest(context.Background(), testRepo); err != nil {
		t.Fatalf("FetchLatest() error = %v", err)
	}
	if auth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer s3cret")
	}
}

func TestFetchLatestPrefersGHTokenOverGitHubToken(t *testing.T) {
	var auth string
	withAPI(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(latestBody))
	})
	t.Setenv("GITHUB_TOKEN", "second")
	t.Setenv("GH_TOKEN", "first")

	if _, err := FetchLatest(context.Background(), testRepo); err != nil {
		t.Fatalf("FetchLatest() error = %v", err)
	}
	if auth != "Bearer first" {
		t.Errorf("Authorization = %q, want GH_TOKEN to win", auth)
	}
}

func TestFetchLatestSendsNoCredentialWhenNoneIsConfigured(t *testing.T) {
	var auth string
	withAPI(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(latestBody))
	})

	if _, err := FetchLatest(context.Background(), testRepo); err != nil {
		t.Fatalf("FetchLatest() error = %v", err)
	}
	if auth != "" {
		t.Errorf("Authorization = %q, want no header", auth)
	}
}

// GitHub returns 404 for a private repository the caller cannot see, so the
// unauthenticated case must say how to authenticate rather than claim there
// are no releases.
func TestFetchLatest404WithoutTokenExplainsHowToAuthenticate(t *testing.T) {
	withAPI(t, serveJSON(http.StatusNotFound, `{"message":"Not Found"}`))

	_, err := FetchLatest(context.Background(), testRepo)
	if err == nil {
		t.Fatal("expected an error for a 404")
	}
	for _, want := range []string{"GH_TOKEN", "gh auth login", testRepo} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	// The message must stay visibility-neutral: asserting "the repository is
	// private" is false for a public repo that was renamed or has no releases
	// yet. Only the conditional form is allowed.
	if strings.Contains(err.Error(), "the repository is private") {
		t.Errorf("error %q asserts the repo is private; it must stay conditional", err)
	}
	if !strings.Contains(err.Error(), "If it is private") {
		t.Errorf("error %q should carry the conditional private-repo hint", err)
	}
}

func TestFetchLatest404WithTokenPointsAtAccess(t *testing.T) {
	withAPI(t, serveJSON(http.StatusNotFound, `{"message":"Not Found"}`))
	t.Setenv("GH_TOKEN", "s3cret")

	_, err := FetchLatest(context.Background(), testRepo)
	if err == nil {
		t.Fatal("expected an error for a 404")
	}
	// With a token in play, telling the user to get a token is useless advice.
	if strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("error %q should not tell an authenticated user to log in", err)
	}
	if !strings.Contains(err.Error(), "read the repository") {
		t.Errorf("error %q should point at repository access", err)
	}
}

func TestFetchLatestRejectedTokenSaysSo(t *testing.T) {
	withAPI(t, serveJSON(http.StatusUnauthorized, `{"message":"Bad credentials"}`))
	t.Setenv("GH_TOKEN", "stale")

	_, err := FetchLatest(context.Background(), testRepo)
	if err == nil {
		t.Fatal("expected an error for a 401")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error %q should say the token was rejected", err)
	}
}

// rateLimitedAPI answers the API like GitHub does once the hourly budget is
// used up, and the release page with a redirect to tag (none when tag is "").
func rateLimitedAPI(reset time.Time, tag string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			w.Header().Set("X-RateLimit-Limit", "60")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		if r.Method != http.MethodHead || r.URL.Path != "/"+testRepo+"/releases/latest" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if tag == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.Redirect(w, r, "/"+testRepo+"/releases/tag/"+tag, http.StatusFound)
	}
}

func TestFetchLatestFallsBackToTheReleasePageWhenRateLimited(t *testing.T) {
	withAPI(t, rateLimitedAPI(time.Now().Add(time.Hour), "v1.4.0"))

	got, err := FetchLatest(context.Background(), testRepo)
	if err != nil {
		t.Fatalf("FetchLatest() error = %v", err)
	}
	if got.Tag != "v1.4.0" {
		t.Errorf("Tag = %q, want v1.4.0", got.Tag)
	}
	if !strings.HasSuffix(got.URL, "/"+testRepo+"/releases/tag/v1.4.0") {
		t.Errorf("URL = %q, want the release's tag page", got.URL)
	}
}

func TestFetchLatestRateLimitSaysSoWhenThePageCannotHelp(t *testing.T) {
	reset := time.Date(2026, 10, 3, 12, 52, 0, 0, time.Local)
	withAPI(t, rateLimitedAPI(reset, ""))

	_, err := FetchLatest(context.Background(), testRepo)
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("error = %v, want a *RateLimitError", err)
	}
	if !rl.Reset.Equal(reset) {
		t.Errorf("Reset = %v, want %v", rl.Reset, reset)
	}
	for _, want := range []string{"rate limit", "12:52", "GH_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// A stale token on a public repository is refused by the API, but the
// release page needs no token at all.
func TestFetchLatestRejectedTokenFallsBackToTheReleasePage(t *testing.T) {
	withAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the release page was sent credentials")
		}
		http.Redirect(w, r, "/"+testRepo+"/releases/tag/v2.0.0", http.StatusFound)
	})
	t.Setenv("GH_TOKEN", "stale")

	got, err := FetchLatest(context.Background(), testRepo)
	if err != nil {
		t.Fatalf("FetchLatest() error = %v", err)
	}
	if got.Tag != "v2.0.0" {
		t.Errorf("Tag = %q, want v2.0.0", got.Tag)
	}
}

// A 404 is GitHub answering, not declining to: the page would say the same.
func TestFetchLatest404DoesNotConsultTheReleasePage(t *testing.T) {
	var pageHits int
	withAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/") {
			pageHits++
		}
		w.WriteHeader(http.StatusNotFound)
	})

	if _, err := FetchLatest(context.Background(), testRepo); err == nil {
		t.Fatal("expected an error for a 404")
	}
	if pageHits != 0 {
		t.Errorf("release page consulted %d time(s) after a 404", pageHits)
	}
}

func TestLatestFromWebWithoutReleasesIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GitHub sends /releases/latest to the bare /releases page then.
		http.Redirect(w, r, "/"+testRepo+"/releases", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	if got, err := LatestFromWeb(context.Background(), srv.URL, testRepo); err == nil {
		t.Fatalf("LatestFromWeb() = %+v, want an error when there is no release", got)
	}
}

func TestRateLimited(t *testing.T) {
	tests := []struct {
		name    string
		code    int
		headers map[string]string
		want    bool
	}{
		{"primary limit", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, true},
		{"secondary limit", http.StatusForbidden, map[string]string{"Retry-After": "30"}, true},
		{"429", http.StatusTooManyRequests, nil, true},
		{"403 with budget left is access", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "12"}, false},
		{"bare 403 is access", http.StatusForbidden, nil, false},
		{"not a refusal", http.StatusNotFound, map[string]string{"X-RateLimit-Remaining": "0"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tt.code, Header: http.Header{}}
			for k, v := range tt.headers {
				resp.Header.Set(k, v)
			}
			if got := RateLimited(resp, false) != nil; got != tt.want {
				t.Errorf("RateLimited() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRateLimitErrorWithTokenDoesNotSuggestOne(t *testing.T) {
	msg := (&RateLimitError{Authenticated: true}).Error()
	if strings.Contains(msg, "GH_TOKEN") {
		t.Errorf("error %q suggests a token to someone already using one", msg)
	}
}

func TestFetchLatestMissingTagIsAnError(t *testing.T) {
	withAPI(t, serveJSON(http.StatusOK, `{"name":"no tag here"}`))

	if _, err := FetchLatest(context.Background(), testRepo); err == nil {
		t.Fatal("expected an error when the response has no tag_name")
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name   string
		local  string
		latest string
		want   Status
	}{
		{"identical tags", "v1.2.3", "v1.2.3", StatusUpToDate},
		{"v prefix only on one side", "1.2.3", "v1.2.3", StatusUpToDate},
		{"older patch", "v1.2.2", "v1.2.3", StatusOutdated},
		{"older minor", "v1.1.9", "v1.2.0", StatusOutdated},
		{"older major", "v0.9.9", "v1.0.0", StatusOutdated},
		{"newer than published", "v1.3.0", "v1.2.3", StatusAhead},
		{"dev build", "dev", "v1.2.3", StatusDevelopment},
		{"go install pseudo version", "(devel)", "v1.2.3", StatusDevelopment},
		{"empty local", "", "v1.2.3", StatusDevelopment},
		{"git describe on the release commit", "v1.2.3-5-gabc1234", "v1.2.3", StatusDevelopment},
		{"unparseable local", "banana", "v1.2.3", StatusOutdated},
		// A latest tag that is not a version must never read as "newer":
		// upgrade installs on StatusOutdated, so this is the difference
		// between a stray release being ignored and being rolled out.
		{"unparseable latest", "v1.2.3", "definitely-no-such-tag-zzz", StatusUnknown},
		{"unparseable both sides", "banana", "zzz", StatusUnknown},
		{"latest is a branch name", "v0.1.22", "main", StatusUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Compare(tt.local, tt.latest); got != tt.want {
				t.Errorf("Compare(%q, %q) = %v, want %v", tt.local, tt.latest, got, tt.want)
			}
		})
	}
}

// The hint is what the user is told to type, and it has to be a command viti
// actually has. It is not a curl|bash line, because plugins ship no
// installer script of their own.
func TestUpgradeHintNamesThePluginCommand(t *testing.T) {
	if got := UpgradeHint("example"); got != "viti plugin upgrade example" {
		t.Errorf("UpgradeHint() = %q, want %q", got, "viti plugin upgrade example")
	}
}

func TestReleasesURLPointsAtTheRepo(t *testing.T) {
	if want := "https://github.com/" + testRepo + "/releases"; ReleasesURL(testRepo) != want {
		t.Errorf("ReleasesURL() = %q, want %q", ReleasesURL(testRepo), want)
	}
}
