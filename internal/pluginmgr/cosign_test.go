package pluginmgr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every real release ships a .cosign.bundle next to each archive, so its
// absence is a signal about the release, not a local inconvenience to skip
// past. This must hold on a machine without cosign too — that is exactly the
// machine that has no other line of defence.
func TestVerifyCosignFailsWhenTheBundleIsMissing(t *testing.T) {
	clearTokenEnv(t)

	dl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(dl.Close)
	old := githubDownloadBase
	githubDownloadBase = dl.URL
	t.Cleanup(func() { githubDownloadBase = old })

	tmp := t.TempDir()
	archive := filepath.Join(tmp, "thing.tar.gz")
	if err := os.WriteFile(archive, []byte("not really a tarball"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr strings.Builder
	entry := &Entry{Repo: "o/r"}
	err := verifyCosign(context.Background(), &stderr, entry, "v1.0.0", "thing.tar.gz", archive, entry.CosignIdentity(), tmp)
	if err == nil {
		t.Fatalf("verifyCosign() = nil for a release with no bundle; stderr:\n%s", stderr.String())
	}
	if !strings.Contains(err.Error(), "bundle") {
		t.Errorf("error should name the missing bundle, got: %v", err)
	}
}
