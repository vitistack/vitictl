package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vitistack/vitictl/internal/release"
)

// The startup hint is the first place a bad release pointer shows up. It must
// warn, and it must not tell people to upgrade.
func TestPrintReleaseStatusWarnsWhenLatestIsNotAVersion(t *testing.T) {
	var buf bytes.Buffer
	latest := &release.Latest{Tag: "definitely-no-such-tag-zzz", URL: "https://example.test/r"}
	printReleaseStatus(&buf, "v0.0.38", latest)
	got := buf.String()
	if !strings.Contains(got, "not a version") || !strings.Contains(got, "definitely-no-such-tag-zzz") {
		t.Errorf("should warn that the latest tag is not a version, got:\n%s", got)
	}
	for _, absent := range []string{"newer release", "viti upgrade", "upgrade with"} {
		if strings.Contains(got, absent) {
			t.Errorf("must not prompt an upgrade (%q), got:\n%s", absent, got)
		}
	}
}

func TestPrintReleaseStatusOutdatedStillPromptsAnUpgrade(t *testing.T) {
	var buf bytes.Buffer
	printReleaseStatus(&buf, "v0.0.37", &release.Latest{Tag: "v0.0.38", URL: "https://example.test/r"})
	if got := buf.String(); !strings.Contains(got, "newer release") || !strings.Contains(got, "viti upgrade") {
		t.Errorf("outdated should prompt an upgrade, got:\n%s", got)
	}
}
