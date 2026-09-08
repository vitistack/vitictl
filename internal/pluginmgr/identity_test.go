package pluginmgr

import (
	"regexp"
	"testing"
)

// cosign applies --certificate-identity-regexp with Go's regexp against the
// certificate SAN, so matching it here exercises the same semantics.
func TestCosignIdentityDefaultAcceptsOnlyVersionTags(t *testing.T) {
	e := &Entry{Repo: "vitistack/vitictl-nhn"}
	re := regexp.MustCompile(e.CosignIdentity())
	san := func(ref string) string {
		return "https://github.com/vitistack/vitictl-nhn/.github/workflows/release.yml@refs/tags/" + ref
	}

	for _, ok := range []string{"v0.1.22", "v10.0.0", "v0.2.0-rc1"} {
		if !re.MatchString(san(ok)) {
			t.Errorf("identity %q rejects a real release tag %q", re, ok)
		}
	}
	// A release published under any other name is signed by the same
	// workflow, so only the tag shape distinguishes it from a real one.
	for _, bad := range []string{"definitely-no-such-tag-zzz", "main", "v1.2", "0.1.22", "v0.1.22/extra"} {
		if re.MatchString(san(bad)) {
			t.Errorf("identity %q accepts non-version tag %q", re, bad)
		}
	}
}

func TestCosignIdentityExplicitOverrideWins(t *testing.T) {
	e := &Entry{Repo: "x/y", CosignIdentityRegex: "^custom$"}
	if got := e.CosignIdentity(); got != "^custom$" {
		t.Errorf("CosignIdentity() = %q, want the explicit override", got)
	}
}
